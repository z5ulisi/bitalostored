// Copyright 2017-2020 Lei Ni (nilei81@gmail.com), Bitalostored author and other contributors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
//
// The implementation of the LogReader struct below is influenced by
// CockroachDB's replicaRaftStorage.
//
// Copyright 2015 The Cockroach Authors.
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

package logdb

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/zuoyebang/bitalostored/raft/internal/raft"
	"github.com/zuoyebang/bitalostored/raft/raftio"
	pb "github.com/zuoyebang/bitalostored/raft/raftpb"

	"github.com/lni/goutils/logutil"
)

const (
	maxEntrySliceSize uint64 = 4 * 1024 * 1024
)

var dn = logutil.DescribeNode

// LogReader is the struct used to manage logs that have already been persisted
// into LogDB. This implementation is influenced by CockroachDB's
// replicaRaftStorage.
type LogReader struct {
	sync.Mutex
	logdb       raftio.ILogDB
	compactor   pb.ICompactor
	snapshot    pb.Snapshot
	state       pb.State
	markerIndex uint64
	clusterID   uint64
	nodeID      uint64
	markerTerm  uint64
	length      uint64
}

var _ raft.ILogDB = (*LogReader)(nil)

// NewLogReader creates and returns a new LogReader instance.
func NewLogReader(clusterID uint64,
	nodeID uint64, logdb raftio.ILogDB) *LogReader {
	l := &LogReader{
		logdb:     logdb,
		clusterID: clusterID,
		nodeID:    nodeID,
		length:    1,
	}
	return l
}

// SetCompactor sets the compactor or the LogReader instance.
func (lr *LogReader) SetCompactor(c pb.ICompactor) {
	if lr.compactor != nil {
		panic("compactor already set")
	}
	lr.compactor = c
}

func (lr *LogReader) id() string {
	return fmt.Sprintf("logreader %s index %d term %d length %d",
		dn(lr.clusterID, lr.nodeID), lr.markerIndex, lr.markerTerm, lr.length)
}

// NodeState returns the initial state.
func (lr *LogReader) NodeState() (pb.State, pb.Membership) {
	lr.Lock()
	defer lr.Unlock()
	return lr.state, lr.snapshot.Membership
}

// Entries returns persisted entries between [low, high) with a total limit of
// up to maxSize bytes.
func (lr *LogReader) Entries(low uint64,
	high uint64, maxSize uint64) ([]pb.Entry, error) {
	ents, size, err := lr.entries(low, high, maxSize)
	if err != nil {
		return nil, err
	}
	if maxSize > 0 && size > maxSize && len(ents) > 1 {
		return ents[:len(ents)-1], nil
	} else if maxSize == 0 && size > maxSize && len(ents) > 1 {
		return ents[:1], nil
	}
	return ents, nil
}

func (lr *LogReader) entries(low uint64,
	high uint64, maxSize uint64) ([]pb.Entry, uint64, error) {
	lr.Lock()
	defer lr.Unlock()
	return lr.entriesLocked(low, high, maxSize)
}

func (lr *LogReader) entriesLocked(low uint64,
	high uint64, maxSize uint64) ([]pb.Entry, uint64, error) {
	if low > high {
		return nil, 0, fmt.Errorf("high (%d) < low (%d)", high, low)
	}

	if low <= lr.markerIndex {
		return nil, 0, raft.ErrCompacted
	}

	if high > lr.lastIndex()+1 {
		plog.Errorf("%s, low %d high %d, lastIndex %d",
			lr.id(), low, high, lr.lastIndex())
		return nil, 0, raft.ErrUnavailable
	}
	// limit the size the ents slice to handle the extreme situation in which
	// high-low can be tens of millions, slice cap is > 50,000 when
	// maxEntrySliceSize is 4MBytes
	maxEntries := maxEntrySliceSize / uint64(unsafe.Sizeof(pb.Entry{}))
	if high-low > maxEntries {
		high = low + maxEntries
		plog.Warningf("%s limited high to %d in logReader.entriesLocked", lr.id(), high)
	}
	ents := make([]pb.Entry, 0, high-low)
	size := uint64(0)
	hitIndex := low

	// lr.readEntryByIndex(low, high) // debug

	ents, size, err := lr.logdb.IterateEntries(ents, size, lr.clusterID,
		lr.nodeID, hitIndex, high, maxSize)
	if err != nil {
		return nil, 0, err
	}
	if uint64(len(ents)) == high-low || size > maxSize {
		return ents, size, nil
	}
	if len(ents) > 0 {
		if ents[0].Index > low {
			return nil, 0, raft.ErrCompacted
		}
		expected := ents[len(ents)-1].Index + 1
		if lr.lastIndex() <= expected {
			plog.Errorf("%s, %v, low %d high %d, expected %d, lastIndex %d",
				lr.id(), raft.ErrUnavailable, low, high, expected, lr.lastIndex())
			return nil, 0, raft.ErrUnavailable
		}
		return nil, 0, fmt.Errorf("gap found between [%d:%d) at %d",
			low, high, expected)
	}

	plog.Warningf("%s failed to get anything from logreader. high:%d low:%d num:%d reaadsize:%d maxSize:%d", lr.id(), high, low, len(ents), size, maxSize)

	return nil, 0, raft.ErrUnavailable
}

// Term returns the term of the entry specified by the entry index.
func (lr *LogReader) Term(index uint64) (uint64, error) {
	lr.Lock()
	defer lr.Unlock()
	return lr.termLocked(index)
}

func (lr *LogReader) termLocked(index uint64) (uint64, error) {
	if index == lr.markerIndex {
		t := lr.markerTerm
		return t, nil
	}
	ents, _, err := lr.entriesLocked(index, index+1, 0)
	if err != nil {
		return 0, err
	}
	if len(ents) == 0 {
		return 0, nil
	}
	return ents[0].Term, nil
}

// GetRange returns the index range of all logs managed by the LogReader
// instance.
func (lr *LogReader) GetRange() (uint64, uint64) {
	lr.Lock()
	defer lr.Unlock()

	return lr.firstIndex(), lr.lastIndex()
}

func (lr *LogReader) firstIndex() uint64 {
	return lr.markerIndex + 1
}

func (lr *LogReader) lastIndex() uint64 {
	return lr.markerIndex + lr.length - 1
}

// TODO: check where this method is called, double check whether
// Unref() got called as expected

// Snapshot returns the metadata of the lastest snapshot.
func (lr *LogReader) Snapshot() pb.Snapshot {
	lr.Lock()
	defer lr.Unlock()
	ss := lr.snapshot
	if !pb.IsEmptySnapshot(ss) {
		ss.Ref()
	}
	return ss
}

// ApplySnapshot applies the specified snapshot.
func (lr *LogReader) ApplySnapshot(snapshot pb.Snapshot) error {
	lr.Lock()
	defer lr.Unlock()
	if err := lr.setSnapshot(snapshot); err != nil {
		return err
	}
	lr.markerIndex = snapshot.Index
	lr.markerTerm = snapshot.Term
	lr.length = 1

	plog.Infof("LogReader ApplySnapshot markerIndex:%d, markerTerm:%d", lr.markerIndex, lr.markerTerm)
	return nil
}

// CreateSnapshot keeps the metadata of the specified snapshot.
func (lr *LogReader) CreateSnapshot(snapshot pb.Snapshot) error {
	lr.Lock()
	defer lr.Unlock()
	return lr.setSnapshot(snapshot)
}

func (lr *LogReader) setSnapshot(snapshot pb.Snapshot) error {
	if lr.snapshot.Index >= snapshot.Index {
		plog.Debugf("%s called setSnapshot, existing %d, new %d",
			lr.id(), lr.snapshot.Index, snapshot.Index)
		return raft.ErrSnapshotOutOfDate
	}
	snapshot.Load(lr.compactor)
	if !pb.IsEmptySnapshot(lr.snapshot) {
		plog.Debugf("%s unref snapshot %d", lr.id(), lr.snapshot.Index)
		if err := lr.snapshot.Unref(); err != nil {
			return err
		}
	}
	plog.Debugf("%s set snapshot %d", lr.id(), snapshot.Index)
	lr.snapshot = snapshot
	return nil
}

// Append marks the specified entries as persisted and make them available from
// logreader.
func (lr *LogReader) Append(entries []pb.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	if len(entries) > 0 {
		if entries[0].Index+uint64(len(entries))-1 != entries[len(entries)-1].Index {
			panic("gap in entries")
		}
	}
	lr.SetRange(entries[0].Index, uint64(len(entries)))
	return nil
}

// SetRange updates the LogReader to reflect what is available in it.
func (lr *LogReader) SetRange(firstIndex uint64, length uint64) {
	if length == 0 {
		return
	}
	lr.Lock()
	defer lr.Unlock()
	first := lr.firstIndex()
	last := firstIndex + length - 1
	if last < first {
		return
	}
	if first > firstIndex {
		cut := first - firstIndex
		firstIndex = first
		length -= cut
	}
	offset := firstIndex - lr.markerIndex
	switch {
	case lr.length > offset:
		lr.length = offset + length
	case lr.length == offset:
		lr.length += length
	default:
		plog.Panicf("%s gap in log entries, marker %d, len %d, first %d, len %d",
			lr.id(), lr.markerIndex, lr.length, firstIndex, length)
	}
}

// SetState sets the persistent state.
func (lr *LogReader) SetState(s pb.State) {
	lr.Lock()
	defer lr.Unlock()
	lr.state = s
}

func (lr *LogReader) SetCommitIndex(index uint64) {
	lr.Lock()
	defer lr.Unlock()
	lr.state.Commit = index
}

func (lr *LogReader) SetMarkerIndex(index uint64) {
	lr.Lock()
	defer lr.Unlock()
	lr.markerIndex = index
}

func (lr *LogReader) GetMarkerIndex() uint64 {
	lr.Lock()
	defer lr.Unlock()
	return lr.markerIndex
}

// Compact compacts raft log entries up to index.
func (lr *LogReader) Compact(index uint64) error {
	lr.Lock()
	defer lr.Unlock()
	if index < lr.markerIndex {
		return raft.ErrCompacted
	}
	if index > lr.lastIndex() {
		return raft.ErrUnavailable
	}
	term, err := lr.termLocked(index)
	if err != nil {
		return err
	}
	i := index - lr.markerIndex
	lr.length -= i
	lr.markerIndex = index
	lr.markerTerm = term
	return nil
}

func (lr *LogReader) readEntryByIndex(low, high uint64) {
	minIndex := low
	maxIndex := high

	step := uint64(10000)
	findMin := minIndex
	findMax := minIndex
	if low > high {
		return
	}

	for {
		findMax = findMin + step
		if findMax > maxIndex {
			findMax = maxIndex
		}

		plog.Infof("scan entries from index: %d to %d", findMin, findMax)

		readEnts := make([]pb.Entry, 0, findMax-findMin)
		readSize := uint64(0)
		readEnts, readSize, err := lr.logdb.IterateEntries(readEnts, readSize, lr.clusterID,
			lr.nodeID, findMin, findMax, 20<<20)
		_, _, _ = readEnts, readSize, err
		plog.Infof("entry scan. num: %d readSize: %d err: %v", len(readEnts), readSize, err)

		for _, e := range readEnts {
			plog.Infof("entry scan. index: %d", e.Index)
		}

		plog.Infof("scan %d entries", findMax-findMin)
		findMin = findMax
		if findMax == maxIndex {
			break
		}
	}
}