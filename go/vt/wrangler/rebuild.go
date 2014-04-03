// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wrangler

import (
	"fmt"
	"sync"

	log "github.com/golang/glog"
	"github.com/youtube/vitess/go/vt/concurrency"
	"github.com/youtube/vitess/go/vt/key"
	"github.com/youtube/vitess/go/vt/tabletmanager/actionnode"
	"github.com/youtube/vitess/go/vt/topo"
)

// Rebuild the serving and replication rollup data data while locking
// out other changes.
func (wr *Wrangler) RebuildShardGraph(keyspace, shard string, cells []string) error {
	actionNode := actionnode.RebuildShard()
	lockPath, err := wr.lockShard(keyspace, shard, actionNode)
	if err != nil {
		return err
	}

	err = topo.RebuildShard(wr.ts, keyspace, shard, topo.RebuildShardOptions{Cells: cells, IgnorePartialResult: false})
	return wr.unlockShard(keyspace, shard, actionNode, lockPath, err)
}

// Rebuild the serving graph data while locking out other changes.
func (wr *Wrangler) RebuildKeyspaceGraph(keyspace string, cells []string) error {
	actionNode := actionnode.RebuildKeyspace()
	lockPath, err := wr.lockKeyspace(keyspace, actionNode)
	if err != nil {
		return err
	}

	err = wr.rebuildKeyspace(keyspace, cells)
	return wr.unlockKeyspace(keyspace, actionNode, lockPath, err)
}

type cellKeyspace struct {
	cell     string
	keyspace string
}

// This function should only be used with an action lock on the keyspace
// - otherwise the consistency of the serving graph data can't be
// guaranteed.
//
// Take data from the global keyspace and rebuild the local serving
// copies in each cell.
func (wr *Wrangler) rebuildKeyspace(keyspace string, cells []string) error {
	log.Infof("rebuildKeyspace %v", keyspace)

	ki, err := wr.ts.GetKeyspace(keyspace)
	if err != nil {
		// Temporary change: we try to keep going even if node
		// doesn't exist
		if err != topo.ErrNoNode {
			return err
		}
		ki = topo.NewKeyspaceInfo(keyspace, &topo.Keyspace{})
	}

	shards, err := wr.ts.GetShardNames(keyspace)
	if err != nil {
		return err
	}

	// Rebuild all shards in parallel.
	wg := sync.WaitGroup{}
	er := concurrency.FirstErrorRecorder{}
	for _, shard := range shards {
		wg.Add(1)
		go func(shard string) {
			if err := wr.RebuildShardGraph(keyspace, shard, cells); err != nil {
				er.RecordError(fmt.Errorf("RebuildShardGraph failed: %v/%v %v", keyspace, shard, err))
			}
			wg.Done()
		}(shard)
	}
	wg.Wait()
	if er.HasErrors() {
		return er.Error()
	}

	// Scan the first shard to discover which cells need local serving data.
	aliases, err := topo.FindAllTabletAliasesInShard(wr.ts, keyspace, shards[0])
	if err != nil {
		return err
	}

	// srvKeyspaceMap is a map:
	//   key: local keyspace {cell,keyspace}
	//   value: topo.SrvKeyspace object being built
	srvKeyspaceMap := make(map[cellKeyspace]*topo.SrvKeyspace)
	for _, alias := range aliases {
		keyspaceLocation := cellKeyspace{alias.Cell, keyspace}
		if _, ok := srvKeyspaceMap[keyspaceLocation]; !ok {
			// before adding keyspaceLocation to the map of
			// of KeyspaceByPath, we check this is a
			// serving tablet. No serving tablet in shard
			// 0 means we're not rebuilding the serving
			// graph in that cell.  This is somewhat
			// expensive, but we only do it on all the
			// non-serving tablets in a shard before we
			// find a serving tablet.
			ti, err := wr.ts.GetTablet(alias)
			if err != nil {
				return err
			}
			if !ti.IsInServingGraph() {
				continue
			}

			srvKeyspaceMap[keyspaceLocation] = &topo.SrvKeyspace{
				Shards:             make([]topo.SrvShard, 0, 16),
				ShardingColumnName: ki.ShardingColumnName,
				ShardingColumnType: ki.ShardingColumnType,
				ServedFrom:         ki.ServedFrom,
			}
		}
	}

	// for each entry in the srvKeyspaceMap map, we do the following:
	// - read the ShardInfo structures for each shard
	// - compute the union of the db types (replica, master, ...)
	// - sort the shards in the list by range
	// - check the ranges are compatible (no hole, covers everything)
	for ck, srvKeyspace := range srvKeyspaceMap {
		keyspaceDbTypes := make(map[topo.TabletType]bool)
		srvKeyspace.Partitions = make(map[topo.TabletType]*topo.KeyspacePartition)
		for _, shard := range shards {
			srvShard, err := wr.ts.GetSrvShard(ck.cell, ck.keyspace, shard)
			if err != nil {
				return err
			}
			for _, tabletType := range srvShard.TabletTypes {
				keyspaceDbTypes[tabletType] = true
			}

			// for each type this shard is supposed to serve,
			// add it to srvKeyspace.Partitions
			for _, tabletType := range srvShard.ServedTypes {
				if _, ok := srvKeyspace.Partitions[tabletType]; !ok {
					srvKeyspace.Partitions[tabletType] = &topo.KeyspacePartition{
						Shards: make([]topo.SrvShard, 0)}
				}
				srvKeyspace.Partitions[tabletType].Shards = append(srvKeyspace.Partitions[tabletType].Shards, *srvShard)
			}
		}

		srvKeyspace.TabletTypes = make([]topo.TabletType, 0, len(keyspaceDbTypes))
		for dbType := range keyspaceDbTypes {
			srvKeyspace.TabletTypes = append(srvKeyspace.TabletTypes, dbType)
		}

		first := true
		for tabletType, partition := range srvKeyspace.Partitions {
			topo.SrvShardArray(partition.Shards).Sort()

			// check the first Start is MinKey, the last End is MaxKey,
			// and the values in between match: End[i] == Start[i+1]
			if partition.Shards[0].KeyRange.Start != key.MinKey {
				return fmt.Errorf("Keyspace partition for %v does not start with %v", tabletType, key.MinKey)
			}
			if partition.Shards[len(partition.Shards)-1].KeyRange.End != key.MaxKey {
				return fmt.Errorf("Keyspace partition for %v does not end with %v", tabletType, key.MaxKey)
			}
			for i := range partition.Shards[0 : len(partition.Shards)-1] {
				if partition.Shards[i].KeyRange.End != partition.Shards[i+1].KeyRange.Start {
					return fmt.Errorf("Non-contiguous KeyRange values for %v at shard %v to %v: %v != %v", tabletType, i, i+1, partition.Shards[i].KeyRange.End.Hex(), partition.Shards[i+1].KeyRange.Start.Hex())
				}
			}

			// backfill Shards
			if first {
				first = false
				srvKeyspace.Shards = partition.Shards
			}
		}
	}

	// and then finally save the keyspace objects
	for ck, srvKeyspace := range srvKeyspaceMap {
		if err := wr.ts.UpdateSrvKeyspace(ck.cell, ck.keyspace, srvKeyspace); err != nil {
			return fmt.Errorf("writing serving data failed: %v", err)
		}
	}
	return nil
}

// This is a quick and dirty tool to resurrect the TopologyServer data from the
// canonical data stored in the tablet nodes.
//
// cells: local vt cells to scan for all tablets
// keyspaces: list of keyspaces to rebuild
func (wr *Wrangler) RebuildReplicationGraph(cells []string, keyspaces []string) error {
	if cells == nil || len(cells) == 0 {
		return fmt.Errorf("must specify cells to rebuild replication graph")
	}
	if keyspaces == nil || len(keyspaces) == 0 {
		return fmt.Errorf("must specify keyspaces to rebuild replication graph")
	}

	allTablets := make([]*topo.TabletInfo, 0, 1024)
	for _, cell := range cells {
		tablets, err := GetAllTablets(wr.ts, cell)
		if err != nil {
			return err
		}
		allTablets = append(allTablets, tablets...)
	}

	for _, keyspace := range keyspaces {
		log.V(6).Infof("delete keyspace shards: %v", keyspace)
		if err := wr.ts.DeleteKeyspaceShards(keyspace); err != nil {
			return err
		}
	}

	keyspacesToRebuild := make(map[string]bool)
	shardsCreated := make(map[string]bool)
	hasErr := false
	mu := sync.Mutex{}
	wg := sync.WaitGroup{}
	for _, ti := range allTablets {
		wg.Add(1)
		go func(ti *topo.TabletInfo) {
			defer wg.Done()
			if !ti.IsInReplicationGraph() {
				return
			}
			if !strInList(keyspaces, ti.Keyspace) {
				return
			}
			mu.Lock()
			keyspacesToRebuild[ti.Keyspace] = true
			shardPath := ti.Keyspace + "/" + ti.Shard
			if !shardsCreated[shardPath] {
				if err := topo.CreateShard(wr.ts, ti.Keyspace, ti.Shard); err != nil && err != topo.ErrNodeExists {
					log.Warningf("failed re-creating shard %v: %v", shardPath, err)
					hasErr = true
				} else {
					shardsCreated[shardPath] = true
				}
			}
			mu.Unlock()
			err := topo.CreateTabletReplicationData(wr.ts, ti.Tablet)
			if err != nil {
				mu.Lock()
				hasErr = true
				mu.Unlock()
				log.Warningf("failed creating replication path: %v", err)
			}
		}(ti)
	}
	wg.Wait()

	for keyspace := range keyspacesToRebuild {
		wg.Add(1)
		go func(keyspace string) {
			defer wg.Done()
			if err := wr.RebuildKeyspaceGraph(keyspace, nil); err != nil {
				mu.Lock()
				hasErr = true
				mu.Unlock()
				log.Warningf("RebuildKeyspaceGraph(%v) failed: %v", keyspace, err)
				return
			}
		}(keyspace)
	}
	wg.Wait()

	if hasErr {
		return fmt.Errorf("some errors occurred rebuilding replication graph, consult log")
	}
	return nil
}
