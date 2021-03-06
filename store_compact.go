//  Copyright (c) 2016 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the
//  License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing,
//  software distributed under the License is distributed on an "AS
//  IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
//  express or implied. See the License for the specific language
//  governing permissions and limitations under the License.

package moss

import (
	"fmt"
	"os"
	"time"
)

func (s *Store) compactMaybe(higher Snapshot, persistOptions StorePersistOptions) (
	bool, error) {
	if s.Options().CollectionOptions.ReadOnly {
		// Do not compact in Read-Only mode
		return false, nil
	}

	compactionConcern := persistOptions.CompactionConcern
	if compactionConcern <= 0 {
		return false, nil
	}

	footer, err := s.snapshot()
	if err != nil {
		return false, err
	}

	defer footer.DecRef()

	slocs, _ := footer.segmentLocs()

	defer footer.DecRef()
	var partialCompactStart int

	if compactionConcern == CompactionAllow {
		// First compute size of the incoming batch of data.
		var incomingDataSize uint64
		if higher != nil {
			higherSS, ok := higher.(*segmentStack)
			if ok {
				higherStats := higherSS.Stats()
				if higherStats != nil {
					incomingDataSize = higherStats.CurBytes
				}
			}
		}

		// Try leveled compaction to same file.
		var doCompact bool
		partialCompactStart, doCompact = calcPartialCompactionStart(slocs,
			incomingDataSize, s.options)
		if doCompact {
			compactionConcern = CompactionForce
		} // else append data to the end of the same file.
	}

	if compactionConcern != CompactionForce {
		return false, nil
	}

	err = s.compact(footer, partialCompactStart, higher, persistOptions)
	if err != nil {
		return false, err
	}

	var sizeBefore, sizeAfter int64

	if len(slocs) > 0 {
		mref := slocs[0].mref
		if mref != nil && mref.fref != nil {
			var finfo os.FileInfo
			if partialCompactStart == 0 {
				finfo, err = s.removeFileOnClose(mref.fref)
			} else {
				finfo, err = mref.fref.file.Stat()
			}
			if err == nil && len(finfo.Name()) > 0 {
				// Fetch size of old file
				sizeBefore = finfo.Size()
			}
		}
	}

	slocs, _ = footer.segmentLocs()
	defer footer.DecRef()

	if len(slocs) > 0 {
		mref := slocs[0].mref
		if mref != nil && mref.fref != nil {
			finfo, err := mref.fref.file.Stat()
			if err == nil && len(finfo.Name()) > 0 {
				// Fetch size of new file
				sizeAfter = finfo.Size()
			}
		}
	}

	s.m.Lock()
	s.numLastCompactionBeforeBytes = uint64(sizeBefore)
	s.numLastCompactionAfterBytes = uint64(sizeAfter)
	delta := sizeBefore - sizeAfter
	if delta > 0 {
		s.totCompactionDecreaseBytes += uint64(delta)
		if s.maxCompactionDecreaseBytes < uint64(delta) {
			s.maxCompactionDecreaseBytes = uint64(delta)
		}
	} else if delta < 0 {
		delta = -delta
		s.totCompactionIncreaseBytes += uint64(delta)
		if s.maxCompactionIncreaseBytes < uint64(delta) {
			s.maxCompactionIncreaseBytes = uint64(delta)
		}
	}
	s.m.Unlock()

	return true, nil
}

// calcPartialCompactionStart - returns an index into the segment locations
// from which point it would be better to merge segments into a larger one.
// Return Values:
//  0 => Full compaction into a new file.
// >0 => Partially Compact all the segments starting at this return index,
//       and append  this one segment at the end of the file while retaining all
//       segments before this starting segment.
//  false => append data to the end of the file.
//  true  => perform compaction.
//                                         ||
//                                         ||
//  Say CompactionLevelMaxSegments = 2     ||
//                                         ||
//     ||                  ||              ||
//  || ||               || || ??           ||
//  || ||          ==>  || || ??     ==>   ||
//  || ||               || || ??           ||
//  || ||               || || ??           ||
//  || ||  || ||  ##    || || ??           ||
//  || ||  || ||  ##    || || ??           ||
// ----------------->  ----------->      -----> (new file)
//  level1 level0 new  level1 hits max!  final file
//
func calcPartialCompactionStart(slocs SegmentLocs, newDataSize uint64,
	options *StoreOptions) (compStartIdx int, doCompact bool) {
	maxSegmentsPerLevel := options.CompactionLevelMaxSegments
	if maxSegmentsPerLevel == 0 {
		maxSegmentsPerLevel = DefaultStoreOptions.CompactionLevelMaxSegments
	}
	fragmentationThreshold := options.CompactionPercentage
	if fragmentationThreshold == 0.0 {
		fragmentationThreshold = DefaultStoreOptions.CompactionPercentage
	}
	levelMultiplier := options.CompactionLevelMultiplier
	if levelMultiplier == 0 {
		levelMultiplier = DefaultStoreOptions.CompactionLevelMultiplier
	}
	if len(slocs) < maxSegmentsPerLevel {
		return -1, false // No segments => append to end of same file.
	}

	determineExponent := func(segSize, curLevelSize uint64, curLevel int) int {
		newLevel := curLevel
		growBy := uint64(levelMultiplier)
		for sz := curLevelSize * growBy; sz <= segSize && sz > 0; sz *= growBy {
			newLevel++
		}
		return newLevel
	}

	compStartIdx = -1 // Assume we need to append to end of file.
	// sizeSoFar represents the estimated size of the partially compacted
	// future segment if we were to start compacting from current segment.
	sizeSoFar := newDataSize
	curLevelSize := newDataSize
	curLevel := 0
	numInLevel := 1 // Incoming batch is assumed to be appended in L0.

	for idx := len(slocs) - 1; idx >= 0; idx-- {
		segSize := slocs[idx].TotKeyByte + slocs[idx].TotValByte
		newLevel := determineExponent(segSize, curLevelSize, curLevel)
		if newLevel > curLevel {
			break
		}
		numInLevel++
		sizeSoFar += segSize
		if numInLevel > maxSegmentsPerLevel {
			compStartIdx = idx
			curLevel = determineExponent(sizeSoFar, curLevelSize, curLevel)
			numInLevel = 1
			curLevelSize = sizeSoFar
		}
	}

	if compStartIdx > 0 && fragmentationThreshold > 0 {
		totDataSize := uint64(0)
		for idx := 0; idx < len(slocs); idx++ {
			totDataSize += slocs[idx].TotKeyByte + slocs[idx].TotValByte
		}
		finfo, err := slocs[0].mref.fref.file.Stat()
		if err != nil {
			return 0, true
		}
		predictedDataSize := int64(totDataSize) + int64(newDataSize)
		predictedFileSize := finfo.Size() + int64(curLevelSize)

		staleDataSize := predictedFileSize - predictedDataSize
		fragPercentage := float64(staleDataSize) / float64(predictedFileSize)
		if fragPercentage > fragmentationThreshold {
			return 0, true // File is too fragmented for partial compaction.
		}
	}
	if compStartIdx >= 0 {
		doCompact = true
	}

	return compStartIdx, doCompact
}

func (s *Store) compact(footer *Footer, partialCompactStart int, higher Snapshot,
	persistOptions StorePersistOptions) error {
	startTime := time.Now()

	var newSS *segmentStack
	if higher != nil {
		ssHigher, ok := higher.(*segmentStack)
		if !ok {
			return fmt.Errorf("store: can only compact higher that's a segmentStack")
		}
		ssHigher.ensureFullySorted()
		newSS = s.mergeSegStacks(footer, partialCompactStart, ssHigher)
	} else {
		newSS = footer.ss // Safe as footer ref count is held positive.
	}

	var frefCompact *FileRef
	var fileCompact File
	var err error
	if partialCompactStart == 0 {
		s.m.Lock()
		frefCompact, fileCompact, err = s.startFileLOCKED()
		s.m.Unlock()
	} else {
		frefCompact, fileCompact, err = s.startOrReuseFile()
	}
	if err != nil {
		return err
	}
	defer frefCompact.DecRef()

	compactFooter, err := s.writeSegments(newSS, frefCompact, fileCompact,
		partialCompactStart != 0) // include deletions for partialCompactions.
	if err != nil {
		if partialCompactStart == 0 {
			s.removeFileOnClose(frefCompact)
		}
		return err
	}
	// Prefix restore the footer's partialCompactStart
	if partialCompactStart != 0 {
		compactFooter.spliceFooter(footer, partialCompactStart)
	}

	if s.options != nil && s.options.CompactionSync {
		persistOptions.NoSync = false
	}

	err = s.persistFooter(fileCompact, compactFooter, persistOptions)
	if err != nil {
		if partialCompactStart == 0 {
			s.removeFileOnClose(frefCompact)
		}
		return err
	}

	err = compactFooter.loadSegments(s.options, frefCompact)
	if err != nil {
		if partialCompactStart == 0 {
			s.removeFileOnClose(frefCompact)
		}
		return err
	}

	s.m.Lock()
	footerPrev := s.footer
	s.footer = compactFooter // Owns the frefCompact ref-count.
	if partialCompactStart == 0 {
		s.totCompactions++
	} else {
		s.totCompactionsPartial++
	}
	s.m.Unlock()

	s.histograms["CompactUsecs"].Add(
		uint64(time.Since(startTime).Nanoseconds()/1000), 1)

	if footerPrev != nil {
		footerPrev.DecRef()
	}

	return nil
}

func (s *Store) mergeSegStacks(footer *Footer, splicePoint int,
	higher *segmentStack) *segmentStack {
	var footerSS *segmentStack
	var lenFooterSS int
	if footer != nil && footer.ss != nil {
		footerSS = footer.ss
		lenFooterSS = len(footerSS.a)
	}
	rv := &segmentStack{
		options:  higher.options,
		a:        make([]Segment, 0, len(higher.a)+lenFooterSS),
		incarNum: higher.incarNum,
	}
	if footerSS != nil {
		rv.a = append(rv.a, footerSS.a[splicePoint:]...)
	}
	rv.a = append(rv.a, higher.a...)
	for cName, newStack := range higher.childSegStacks {
		if len(rv.childSegStacks) == 0 {
			rv.childSegStacks = make(map[string]*segmentStack)
		}
		if footer == nil {
			rv.childSegStacks[cName] = s.mergeSegStacks(nil,
				splicePoint, newStack)
			continue
		}

		childFooter, exists := footer.ChildFooters[cName]
		if exists {
			if childFooter.incarNum != higher.incarNum {
				// Fast child collection recreation, must not merge
				// segments from prior incarnation.
				childFooter = nil
			}
		}
		rv.childSegStacks[cName] = s.mergeSegStacks(childFooter,
			splicePoint, newStack)
	}
	return rv
}

func (right *Footer) spliceFooter(left *Footer, splicePoint int) {
	slocs := make([]SegmentLoc, splicePoint, splicePoint+len(right.SegmentLocs))
	copy(slocs, left.SegmentLocs[0:splicePoint])
	slocs = append(slocs, right.SegmentLocs...)
	right.SegmentLocs = slocs

	for cName, childFooter := range right.ChildFooters {
		storeChildFooter, exists := left.ChildFooters[cName]
		if exists {
			if storeChildFooter.incarNum != childFooter.incarNum {
				// Fast child collection recreation, ok to drop store footer's
				// segments from prior incarnation.
				continue
			}
			childFooter.spliceFooter(storeChildFooter, splicePoint)
		}
	}
}

func (s *Store) writeSegments(newSS *segmentStack, frefCompact *FileRef,
	fileCompact File, includeDeletes bool) (compactFooter *Footer, err error) {
	finfo, err := fileCompact.Stat()
	if err != nil {
		return nil, err
	}
	pos := finfo.Size()
	stats := newSS.Stats()

	kvsBegPos := pageAlignCeil(pos)
	bufBegPos := pageAlignCeil(kvsBegPos + 1 + (int64(8+8) * int64(stats.CurOps)))

	compactionBufferPages := 0
	if s.options != nil {
		compactionBufferPages = s.options.CompactionBufferPages
	}
	if compactionBufferPages <= 0 {
		compactionBufferPages = DefaultStoreOptions.CompactionBufferPages
	}
	compactionBufferSize := StorePageSize * compactionBufferPages

	compactWriter := &compactWriter{
		kvsWriter: newBufferedSectionWriter(fileCompact, kvsBegPos, 0, compactionBufferSize, s),
		bufWriter: newBufferedSectionWriter(fileCompact, bufBegPos, 0, compactionBufferSize, s),
	}
	onError := func(err error) error {
		compactWriter.kvsWriter.Stop()
		compactWriter.bufWriter.Stop()
		return err
	}
	s.m.Lock()
	s.totCompactionBeforeBytes += stats.CurBytes
	s.m.Unlock()

	err = newSS.mergeInto(0, len(newSS.a), compactWriter, nil, includeDeletes, false, s.abortCh)
	if err != nil {
		return nil, onError(err)
	}

	if err = compactWriter.kvsWriter.Flush(); err != nil {
		return nil, onError(err)
	}
	if err = compactWriter.bufWriter.Flush(); err != nil {
		return nil, onError(err)
	}

	if err = compactWriter.kvsWriter.Stop(); err != nil {
		return nil, onError(err)
	}
	if err = compactWriter.bufWriter.Stop(); err != nil {
		return nil, onError(err)
	}

	compactFooter = &Footer{
		refs: 1,
		SegmentLocs: []SegmentLoc{
			{
				Kind:       SegmentKindBasic,
				KvsOffset:  uint64(kvsBegPos),
				KvsBytes:   uint64(compactWriter.kvsWriter.Offset() - kvsBegPos),
				BufOffset:  uint64(bufBegPos),
				BufBytes:   uint64(compactWriter.bufWriter.Offset() - bufBegPos),
				TotOpsSet:  compactWriter.totOperationSet,
				TotOpsDel:  compactWriter.totOperationDel,
				TotKeyByte: compactWriter.totKeyByte,
				TotValByte: compactWriter.totValByte,
			},
		},
	}

	for cName, childSegStack := range newSS.childSegStacks {
		if compactFooter.ChildFooters == nil {
			compactFooter.ChildFooters = make(map[string]*Footer)
		}
		childFooter, err := s.writeSegments(childSegStack,
			frefCompact, fileCompact, includeDeletes)
		if err != nil {
			return nil, err
		}
		compactFooter.ChildFooters[cName] = childFooter
	}
	return compactFooter, nil
}

type compactWriter struct {
	file      File
	kvsWriter *bufferedSectionWriter
	bufWriter *bufferedSectionWriter

	totOperationSet   uint64
	totOperationDel   uint64
	totOperationMerge uint64
	totKeyByte        uint64
	totValByte        uint64
}

func (cw *compactWriter) Mutate(operation uint64, key, val []byte) error {
	keyStart := cw.bufWriter.Written()
	_, err := cw.bufWriter.Write(key)
	if err != nil {
		return err
	}

	_, err = cw.bufWriter.Write(val)
	if err != nil {
		return err
	}

	keyLen := len(key)
	valLen := len(val)

	opKlVl := encodeOpKeyLenValLen(operation, keyLen, valLen)

	if keyLen <= 0 && valLen <= 0 {
		keyStart = 0
	}

	pair := []uint64{opKlVl, uint64(keyStart)}
	kvsBuf, err := Uint64SliceToByteSlice(pair)
	if err != nil {
		return err
	}

	_, err = cw.kvsWriter.Write(kvsBuf)
	if err != nil {
		return err
	}

	switch operation {
	case OperationSet:
		cw.totOperationSet++
	case OperationDel:
		cw.totOperationDel++
	case OperationMerge:
		cw.totOperationMerge++
	default:
	}

	cw.totKeyByte += uint64(keyLen)
	cw.totValByte += uint64(valLen)

	return nil
}
