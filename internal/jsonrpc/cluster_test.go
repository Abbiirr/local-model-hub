package jsonrpc

import (
	"context"
	"sort"
	"testing"

	"github.com/localhub/localhub/internal/config"
	"github.com/localhub/localhub/internal/core"
)

// fakeClusterService is a stand-in ClusterService for the mirror-registration tests.
type fakeClusterService struct{ called map[string]int }

func (f *fakeClusterService) NodesList(context.Context) (any, error) {
	f.called["nodes"]++
	return []string{"n1"}, nil
}
func (f *fakeClusterService) ModelsList(context.Context) (any, error) {
	f.called["models"]++
	return []string{"m1"}, nil
}
func (f *fakeClusterService) HardwareGet(context.Context) (any, error) {
	f.called["hardware"]++
	return []string{"hw"}, nil
}

func depless(t *testing.T) *core.Service { t.Helper(); return core.New(core.Deps{Cfg: config.Default()}) }

// TestClusterMirrorsRegisteredOnlyWhenWired proves the P7.2 role-gating of the RPC surface: with NO
// ClusterService (the default NewDispatcher path, and a role=off/node daemon) the cluster.* methods
// are absent; with one wired (a coordinator) exactly the three mirrors appear and reach the service.
func TestClusterMirrorsRegisteredOnlyWhenWired(t *testing.T) {
	// Nil cluster service → no cluster.* methods (mirrors REST role-gating).
	off := NewDispatcher(depless(t))
	for _, m := range off.Methods() {
		if len(m) >= 8 && m[:8] == "cluster." {
			t.Errorf("role-off dispatcher exposes %q; cluster.* must be absent without a ClusterService", m)
		}
	}

	// Wired cluster service → exactly the three mirrors, and they dispatch to the service.
	fake := &fakeClusterService{called: map[string]int{}}
	d := NewDispatcherWithServices(depless(t), unwiredJobService{}, fake)

	var clusterMethods []string
	for _, m := range d.Methods() {
		if len(m) >= 8 && m[:8] == "cluster." {
			clusterMethods = append(clusterMethods, m)
		}
	}
	sort.Strings(clusterMethods)
	want := []string{"cluster.hardware.get", "cluster.models.list", "cluster.nodes.list"}
	if len(clusterMethods) != len(want) {
		t.Fatalf("cluster methods = %v, want %v", clusterMethods, want)
	}
	for i := range want {
		if clusterMethods[i] != want[i] {
			t.Fatalf("cluster methods = %v, want %v", clusterMethods, want)
		}
	}

	ctx := context.Background()
	for _, m := range want {
		res, rpcErr := d.methods[m](ctx, nil)
		if rpcErr != nil {
			t.Errorf("%s returned RPC error %+v, want success reaching the service", m, rpcErr)
		}
		if res == nil {
			t.Errorf("%s returned nil result, want the service value", m)
		}
	}
	if fake.called["nodes"] != 1 || fake.called["models"] != 1 || fake.called["hardware"] != 1 {
		t.Errorf("mirrors did not each reach the service exactly once: %+v", fake.called)
	}
}
