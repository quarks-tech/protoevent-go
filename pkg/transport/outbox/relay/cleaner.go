package relay

import (
	"context"
	"time"
)

// PartitionedStore is an optional interface for stores that support partition truncation.
// Implement this to enable automatic cleanup of old completed messages.
type PartitionedStore interface {
	// TruncateCompletedPartitions truncates the specified hourly partitions (p0-p23).
	// Used for fast cleanup of old completed messages when the completed table
	// is partitioned by HASH(HOUR(sent_time)) PARTITIONS 24.
	TruncateCompletedPartitions(ctx context.Context, partitions ...int) error
}

// CleanerOptions contains configuration for the partition cleaner.
type CleanerOptions struct {
	// RetentionHours specifies how many hours of completed messages to retain.
	// Partitions older than this will be truncated. Must be between 1 and 23.
	RetentionHours int

	// CheckInterval specifies how often to check for partitions to truncate.
	// Default is 1 hour.
	CheckInterval time.Duration

	// Logger for error logging.
	Logger Logger
}

// DefaultCleanerOptions returns the default cleaner configuration.
func DefaultCleanerOptions() CleanerOptions {
	return CleanerOptions{
		RetentionHours: 2,
		CheckInterval:  time.Hour,
	}
}

// CleanerOption configures cleaner options.
type CleanerOption func(*CleanerOptions)

// WithRetentionHours sets the number of hours to retain completed messages.
func WithRetentionHours(hours int) CleanerOption {
	return func(o *CleanerOptions) {
		o.RetentionHours = hours
	}
}

// WithCheckInterval sets how often to check for partitions to truncate.
func WithCheckInterval(d time.Duration) CleanerOption {
	return func(o *CleanerOptions) {
		o.CheckInterval = d
	}
}

// WithCleanerLogger sets the logger for the cleaner.
func WithCleanerLogger(l Logger) CleanerOption {
	return func(o *CleanerOptions) {
		o.Logger = l
	}
}

// PartitionCleaner periodically truncates old hourly partitions from the completed table.
// Run this alongside the relay in an errgroup for automatic cleanup.
//
// Requires the completed table to be partitioned by HASH(HOUR(sent_time)) PARTITIONS 24.
type PartitionCleaner struct {
	store   PartitionedStore
	options CleanerOptions
}

// NewPartitionCleaner creates a new partition cleaner.
func NewPartitionCleaner(store PartitionedStore, opts ...CleanerOption) *PartitionCleaner {
	options := DefaultCleanerOptions()
	for _, opt := range opts {
		opt(&options)
	}

	return &PartitionCleaner{
		store:   store,
		options: options,
	}
}

// Run starts the cleaner loop. It checks once per CheckInterval for partitions to truncate.
// Returns nil on graceful shutdown when context is cancelled.
func (c *PartitionCleaner) Run(ctx context.Context) error {
	// Run once immediately on startup
	_ = c.truncateOldPartitions(ctx)

	ticker := time.NewTicker(c.options.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = c.truncateOldPartitions(ctx)
		}
	}
}

// RunOnce performs a single cleanup check and returns.
// Useful for testing or manual triggering.
func (c *PartitionCleaner) RunOnce(ctx context.Context) error {
	return c.truncateOldPartitions(ctx)
}

func (c *PartitionCleaner) truncateOldPartitions(ctx context.Context) error {
	if c.options.RetentionHours <= 0 || c.options.RetentionHours >= 24 {
		return nil
	}

	currentHour := time.Now().Hour()
	partitions := GetPartitionsToTruncate(currentHour, c.options.RetentionHours)

	if len(partitions) == 0 {
		return nil
	}

	if err := c.store.TruncateCompletedPartitions(ctx, partitions...); err != nil {
		if c.options.Logger != nil {
			c.options.Logger.Errorf("failed to truncate partitions: %v", err)
		}
		return err
	}

	return nil
}

// GetPartitionsToTruncate calculates which hourly partitions should be truncated
// based on the current hour and retention period.
//
// With HASH(HOUR(sent_time)) PARTITIONS 24, partitions are p0-p23 mapping to hours 0-23.
// For example, with currentHour=11 and retentionHours=2, we keep p9,p10,p11 and truncate the rest.
func GetPartitionsToTruncate(currentHour, retentionHours int) []int {
	if retentionHours <= 0 || retentionHours >= 24 {
		return nil
	}

	// Calculate which partitions to keep (current hour and previous retentionHours-1 hours)
	keep := make(map[int]bool)
	for i := range retentionHours {
		hour := (currentHour - i + 24) % 24
		keep[hour] = true
	}

	// Return partitions to truncate
	var truncate []int
	for p := range 24 {
		if !keep[p] {
			truncate = append(truncate, p)
		}
	}

	return truncate
}
