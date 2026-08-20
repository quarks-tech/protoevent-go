package mongodbtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

// ClusterSize is the number of data-bearing members StartCluster boots. Three is the
// smallest set that has a majority to lose: with two members up the set writes, with
// one it does not, so both sides of the majority boundary are reachable.
const ClusterSize = 3

// clusterBasePort is the first port the members listen on, INSIDE the shared network
// namespace and on the host. Deliberately high and unusual to avoid colliding with a
// developer's own mongod on 27017.
const clusterBasePort = 47017

// Cluster is a running three-member replica set with real elections.
//
// It exists because Start boots a SINGLE-node set reached over
// directConnection=true, and on one node majority equals local: there is no
// election, no stepdown, no rollback of a non-majority write, and no clock skew
// between primaries. Every replica-set property the outbox store and the change-stream
// relay depend on — that a token cannot rewind across a failover, that leadership is
// exclusive through one, that delivery STALLS rather than errors when the majority
// commit point stops advancing — is therefore unfalsifiable against Start. The
// directConnection also pins the driver to Single topology, so it would not fail over
// even if given a real set.
//
// All members share ONE network namespace, and that is load-bearing rather than a
// shortcut. A replica set advertises its members' own host:port in the topology the
// driver then dials, so members named by container hostname are unreachable from a
// host-side test, while members named 127.0.0.1 are unreachable from each OTHER when
// each container has its own loopback. Sharing a namespace makes 127.0.0.1:<port>
// mean the same endpoint in both places, which is what lets one address list work
// from the host and between members at once. It also means the ports are FIXED, not
// testcontainers' usual ephemeral mapping — see clusterBasePort.
type Cluster struct {
	Client *mongo.Client
	DB     *mongo.Database

	// members[0] owns the network namespace and the published ports; the others join
	// it. Terminating members[0] therefore takes the whole set down.
	members []testcontainers.Container

	// arbiter records whether the last member is an arbiter, so DataBearingMember can
	// name the one member a majority test may stop.
	arbiter bool

	terminate func()
}

// ClusterOption configures StartCluster.
type ClusterOption func(*clusterConfig)

type clusterConfig struct {
	arbiter bool
}

// WithArbiter makes the LAST member an arbiter, producing a primary-secondary-arbiter
// set instead of three data-bearing members.
//
// PSA is the topology where losing one node produces a SILENT failure rather than a
// loud one. With three data-bearing members, stopping two leaves no majority of votes,
// so the survivor steps down to secondary and every write fails with a
// server-selection error — noisy and obvious. In PSA, stopping the secondary leaves
// primary+arbiter, still a majority of VOTES, so the primary stays primary and keeps
// acknowledging w:1 writes — while the majority COMMIT point, which needs two
// data-bearing acknowledgements, stops advancing. Change streams only deliver
// majority-committed events, so the relay sees empty windows and reports healthy while
// events pile up undelivered.
//
// That asymmetry is why this option exists: it is the only way to stage
// "acknowledged but never delivered, with no error anywhere".
func WithArbiter() ClusterOption {
	return func(c *clusterConfig) { c.arbiter = true }
}

// StartCluster boots a three-member replica set, connects a replicaSet-aware client,
// and returns it with a cleanup. Returns an error wrapping ErrDockerUnavailable when
// Docker is unavailable, exactly as Start does, so callers keep one skip policy.
func StartCluster(ctx context.Context, opts ...ClusterOption) (*Cluster, func(), error) {
	if err := probeDocker(ctx); err != nil {
		return nil, nil, fmt.Errorf("probe docker daemon: %w", errors.Join(ErrDockerUnavailable, err))
	}

	var cfg clusterConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	// The ports are fixed (see clusterBasePort), so two test binaries using this
	// harness collide — and `go test ./...` runs PACKAGES IN PARALLEL, so that is the
	// normal case, not an exotic one. Waiting serializes them; Docker would otherwise
	// report the collision as an opaque bind failure.
	if err := waitForPortsFree(ctx); err != nil {
		return nil, nil, err
	}

	cl := &Cluster{}
	// Unwind whatever came up if a later step fails, so a half-built cluster never
	// leaks containers.
	ok := false
	defer func() {
		if !ok {
			cl.terminateContainers()
		}
	}()

	for i := range ClusterSize {
		c, err := startMember(ctx, i, cl.namespaceOwner())
		if err != nil {
			return nil, nil, fmt.Errorf("start member %d: %w", i, err)
		}
		cl.members = append(cl.members, c)
	}

	if err := initiateReplicaSet(ctx, cl.members[0], cfg.arbiter); err != nil {
		return nil, nil, fmt.Errorf("initiate replica set: %w", err)
	}

	client, err := mongo.Connect(options.Client().ApplyURI(clusterURI()))
	if err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	disconnect := func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dcancel()
		_ = client.Disconnect(dctx)
	}

	// Ping the PRIMARY specifically: a set that has come up but not yet elected is
	// reachable while still refusing writes, and every caller's first act is a write.
	pingCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, readpref.Primary()); err != nil {
		disconnect()

		return nil, nil, fmt.Errorf("ping primary: %w", err)
	}

	cl.Client = client
	cl.DB = client.Database(dbName)
	cl.arbiter = cfg.arbiter
	cl.terminate = func() { disconnect(); cl.terminateContainers() }
	ok = true

	return cl, cl.terminate, nil
}

// namespaceOwner returns the container whose network namespace every later member
// joins, or nil for the first one.
func (c *Cluster) namespaceOwner() testcontainers.Container {
	if len(c.members) == 0 {
		return nil
	}

	return c.members[0]
}

func (c *Cluster) terminateContainers() {
	// Reverse order: the namespace owner is last, since the others' namespace dies
	// with it.
	for _, m := range slices.Backward(c.members) {
		tctx, tcancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = m.Terminate(tctx)
		tcancel()
	}
}

// startMember boots one mongod. The first member publishes EVERY member's port,
// because it owns the namespace they all share; the rest publish nothing and join it.
func startMember(ctx context.Context, index int, owner testcontainers.Container) (testcontainers.Container, error) {
	port := clusterBasePort + index

	req := testcontainers.ContainerRequest{
		Image: "mongo:8",
		Cmd: []string{
			"mongod",
			"--replSet", "rs0",
			"--port", strconv.Itoa(port),
			"--bind_ip_all",
			// A small oplog keeps the resume-token cliff reachable in a test rather
			// than requiring gigabytes of churn to roll it over.
			"--oplogSize", "128",
		},
		WaitingFor: wait.ForLog("Waiting for connections").WithStartupTimeout(180 * time.Second),
	}

	if owner == nil {
		bindings := network.PortMap{}
		loopback := netip.AddrFrom4([4]byte{127, 0, 0, 1})
		for i := range ClusterSize {
			spec := fmt.Sprintf("%d/tcp", clusterBasePort+i)
			p, perr := network.ParsePort(spec)
			if perr != nil {
				return nil, fmt.Errorf("parse port %q: %w", spec, perr)
			}
			bindings[p] = []network.PortBinding{{
				HostIP:   loopback,
				HostPort: strconv.Itoa(clusterBasePort + i),
			}}
			req.ExposedPorts = append(req.ExposedPorts, spec)
		}
		// Fixed host ports, not ephemeral ones: the replica-set config below names
		// these exact numbers, and the driver dials what the set advertises.
		req.HostConfigModifier = func(hc *container.HostConfig) {
			hc.PortBindings = bindings
		}
	} else {
		id := owner.GetContainerID()
		req.HostConfigModifier = func(hc *container.HostConfig) {
			hc.NetworkMode = container.NetworkMode("container:" + id)
		}
	}

	return testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req, Started: true,
	})
}

// initiateReplicaSet configures the set and waits for a primary.
func initiateReplicaSet(ctx context.Context, first testcontainers.Container, arbiter bool) error {
	members := make([]string, 0, ClusterSize)
	for i := range ClusterSize {
		spec := fmt.Sprintf("{_id:%d,host:'127.0.0.1:%d'}", i, clusterBasePort+i)
		if arbiter && i == ClusterSize-1 {
			spec = fmt.Sprintf("{_id:%d,host:'127.0.0.1:%d',arbiterOnly:true}", i, clusterBasePort+i)
		}
		members = append(members, spec)
	}
	cfg := fmt.Sprintf("rs.initiate({_id:'rs0',members:[%s]})", strings.Join(members, ","))

	if out, err := mongosh(ctx, first, cfg); err != nil {
		return fmt.Errorf("rs.initiate: %w (output: %s)", err, out)
	}

	// Poll for a writable primary. rs.initiate returns before the election completes,
	// and every caller's first operation is a write.
	deadline := time.Now().Add(90 * time.Second)
	for {
		out, err := mongosh(ctx, first, "db.hello().isWritablePrimary")
		if err == nil && strings.Contains(out, "true") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no primary was elected within the timeout (last output: %s): %w", out, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// mongosh runs one JavaScript expression against the member's own port.
func mongosh(ctx context.Context, c testcontainers.Container, script string) (string, error) {
	code, reader, err := c.Exec(ctx, []string{
		"mongosh", "--quiet", "--port", strconv.Itoa(clusterBasePort), "--eval", script,
	})
	if err != nil {
		return "", err
	}
	out, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return string(out), fmt.Errorf("mongosh exited %d", code)
	}

	return string(out), nil
}

// clusterURI is the seed list the host-side driver uses. replicaSet (and NOT
// directConnection) is the point: it makes the driver discover the topology and fail
// over, which is what Start's single node cannot do.
func clusterURI() string {
	hosts := make([]string, 0, ClusterSize)
	for i := range ClusterSize {
		hosts = append(hosts, fmt.Sprintf("127.0.0.1:%d", clusterBasePort+i))
	}

	return "mongodb://" + strings.Join(hosts, ",") + "/?replicaSet=rs0"
}

// StepDownPrimary forces the current primary to step down, triggering a real
// election. The change stream's cursor is killed, in-flight majority writes fail with
// a retryable error, and the new primary's clock is its own — the combination a
// relay's failover handling has to survive.
func (c *Cluster) StepDownPrimary(t *testing.T, seconds int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// replSetStepDown severs the connection it is issued on, so an error here is
	// expected rather than a failure; what matters is that a NEW primary appears.
	_, _ = mongosh(ctx, c.members[0],
		fmt.Sprintf("try{rs.stepDown(%d)}catch(e){}", seconds))

	c.WaitForPrimary(t)
}

// WaitForPrimary blocks until the set has a writable primary again.
func (c *Cluster) WaitForPrimary(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	deadline := time.Now().Add(120 * time.Second)
	for {
		if err := c.Client.Ping(ctx, readpref.Primary()); err == nil {
			var res bson.M
			if err := c.Client.Database("admin").
				RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&res); err == nil {
				if w, ok := res["isWritablePrimary"].(bool); ok && w {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the replica set did not elect a writable primary within the timeout")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// StopMember stops one member, for tests that need to remove the majority. Stopping
// member 0 tears down the shared namespace and therefore the whole set, so it is
// rejected: the two stoppable members are exactly the ones a majority test needs.
func (c *Cluster) StopMember(t *testing.T, index int) {
	t.Helper()

	if index <= 0 || index >= ClusterSize {
		t.Fatalf("StopMember(%d): only members 1..%d can be stopped; member 0 owns the shared "+
			"network namespace and stopping it would take the whole set down", index, ClusterSize-1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	timeout := 10 * time.Second
	if err := c.members[index].Stop(ctx, &timeout); err != nil {
		t.Fatalf("stop member %d: %v", index, err)
	}
}

// StartMember brings back a member stopped by StopMember.
func (c *Cluster) StartMember(t *testing.T, index int) {
	t.Helper()

	if index <= 0 || index >= ClusterSize {
		t.Fatalf("StartMember(%d): index out of range", index)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if err := c.members[index].Start(ctx); err != nil {
		t.Fatalf("start member %d: %v", index, err)
	}
}

// SecondaryIndex is the index of the data-bearing secondary — the member to stop when
// a test wants to strand the majority commit point. In a PSA set that is member 1: 0
// owns the shared namespace and the last member is the arbiter.
func (c *Cluster) SecondaryIndex(t *testing.T) int {
	t.Helper()

	if !c.arbiter {
		t.Fatal("SecondaryIndex is meaningful only for a PSA set; build one with WithArbiter()")
	}

	return 1
}

// waitForPortsFree blocks until the fixed ports are free, so a concurrently-running
// test binary using this harness serializes behind the one that got there first
// instead of failing.
func waitForPortsFree(ctx context.Context) error {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		err := checkPortsFree(ctx)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w; if nothing else is running, a cluster from an interrupted run is "+
				"still up (docker ps, then docker rm -f)", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// checkPortsFree reports whether the cluster's fixed ports are available.
func checkPortsFree(ctx context.Context) error {
	var d net.Dialer

	for i := range ClusterSize {
		addr := fmt.Sprintf("127.0.0.1:%d", clusterBasePort+i)

		dctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		c, err := d.DialContext(dctx, "tcp", addr)
		cancel()
		if err != nil {
			continue // nothing listening, which is what we want
		}
		_ = c.Close()

		return fmt.Errorf("%s is already in use: this harness binds fixed ports %d-%d",
			addr, clusterBasePort, clusterBasePort+ClusterSize-1)
	}

	return nil
}
