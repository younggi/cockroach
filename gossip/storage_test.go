// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package gossip_test

import (
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/gossip"
	"github.com/cockroachdb/cockroach/gossip/resolver"
	"github.com/cockroachdb/cockroach/gossip/simulation"
	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/leaktest"
)

type testStorage struct {
	sync.Mutex
	read, write bool
	info        gossip.BootstrapInfo
}

func (ts *testStorage) isRead() bool {
	ts.Lock()
	defer ts.Unlock()
	return ts.read
}

func (ts *testStorage) isWrite() bool {
	ts.Lock()
	defer ts.Unlock()
	return ts.write
}

func (ts *testStorage) Len() int {
	ts.Lock()
	defer ts.Unlock()
	return len(ts.info.Addresses)
}

func (ts *testStorage) ReadBootstrapInfo(info *gossip.BootstrapInfo) error {
	ts.Lock()
	defer ts.Unlock()
	ts.read = true
	*info = *util.CloneProto(&ts.info).(*gossip.BootstrapInfo)
	return nil
}

func (ts *testStorage) WriteBootstrapInfo(info *gossip.BootstrapInfo) error {
	ts.Lock()
	defer ts.Unlock()
	ts.write = true
	ts.info = *util.CloneProto(info).(*gossip.BootstrapInfo)
	return nil
}

type unresolvedAddrSlice []util.UnresolvedAddr

func (s unresolvedAddrSlice) Len() int {
	return len(s)
}
func (s unresolvedAddrSlice) Less(i, j int) bool {
	networkCmp := strings.Compare(s[i].Network(), s[j].Network())
	return networkCmp < 0 || networkCmp == 0 && strings.Compare(s[i].String(), s[j].String()) < 0
}
func (s unresolvedAddrSlice) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// TestGossipStorage verifies that a gossip node can join the cluster
// using the bootstrap hosts in a gossip.Storage object.
func TestGossipStorage(t *testing.T) {
	defer leaktest.AfterTest(t)()

	network := simulation.NewNetwork(3)
	defer network.Stop()

	// Set storage for each of the nodes.
	addresses := make(unresolvedAddrSlice, len(network.Nodes))
	stores := make([]*testStorage, len(network.Nodes))
	for i, n := range network.Nodes {
		addresses[i] = util.MakeUnresolvedAddr(n.Addr.Network(), n.Addr.String())
		stores[i] = new(testStorage)
		if err := n.Gossip.SetStorage(stores[i]); err != nil {
			t.Fatal(err)
		}
	}

	// Wait for the gossip network to connect.
	network.RunUntilFullyConnected()

	// Wait long enough for storage to get the expected number of addresses.
	util.SucceedsSoon(t, func() error {
		for _, p := range stores {
			if p.Len() != 2 {
				return util.Errorf("incorrect number of addresses: expected 2; got %d", p.Len())
			}
		}
		return nil
	})

	for i, p := range stores {
		if !p.isRead() {
			t.Errorf("%d: expected read from storage", i)
		}
		if !p.isWrite() {
			t.Errorf("%d: expected write from storage", i)
		}

		p.Lock()
		gotAddresses := unresolvedAddrSlice(p.info.Addresses)
		sort.Sort(gotAddresses)
		var expectedAddresses unresolvedAddrSlice
		for j, addr := range addresses {
			if i != j { // skip node's own address
				expectedAddresses = append(expectedAddresses, addr)
			}
		}
		sort.Sort(expectedAddresses)

		// Verify all gossip addresses are written to each persistent store.
		if !reflect.DeepEqual(gotAddresses, expectedAddresses) {
			t.Errorf("%d: expected addresses: %s, got: %s", i, expectedAddresses, gotAddresses)
		}
		p.Unlock()
	}

	// Create an unaffiliated gossip node with only itself as a resolver,
	// leaving it no way to reach the gossip network.
	node, err := network.CreateNode()
	if err != nil {
		t.Fatal(err)
	}
	node.Gossip.SetBootstrapInterval(1 * time.Millisecond)

	r, err := resolver.NewResolverFromAddress(node.Addr)
	if err != nil {
		t.Fatal(err)
	}
	node.Gossip.SetResolvers([]resolver.Resolver{r})
	if err := network.StartNode(node); err != nil {
		t.Fatal(err)
	}

	// Wait for a bit to ensure no connection.
	select {
	case <-time.After(10 * time.Millisecond):
		// expected outcome...
	case <-node.Gossip.Connected:
		t.Fatal("unexpectedly connected to gossip")
	}

	// Give the new node storage with info established from a node
	// in the established network.
	var ts2 testStorage
	if err := stores[0].ReadBootstrapInfo(&ts2.info); err != nil {
		t.Fatal(err)
	}
	if err := node.Gossip.SetStorage(&ts2); err != nil {
		t.Fatal(err)
	}

	network.SimulateNetwork(func(cycle int, network *simulation.Network) bool {
		if cycle > 1000 {
			t.Fatal("failed to connect to gossip")
		}
		select {
		case <-node.Gossip.Connected:
			return false
		default:
			return true
		}
	})
}
