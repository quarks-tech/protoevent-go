// Package outbox implements the transactional-outbox pattern for the
// protoevent event bus: events are written to a database log in the same
// transaction as the business change, and a separate relay forwards them to
// the downstream transport with at-least-once delivery.
//
// This package holds the storage-agnostic engine — the Message model, the
// Store publish contract, the Sender/PublisherFactory publish path, and ID
// generation. Store backends live in the tidb and mongodb submodules; the
// relay runtimes live in relay/sequence (TiDB sequenced log) and
// relay/stream (MongoDB change stream). See README.md in this directory for
// the full guide.
package outbox
