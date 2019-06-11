// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file.

package storageclient

import (
	"errors"
	"fmt"
	"github.com/DxChainNetwork/godx/storage"
	"github.com/DxChainNetwork/godx/storage/storageclient/filesystem/dxfile"
	"io"
	"math/rand"
	"os"
	"sync"
	"time"
)

// uploadSegmentID is a unique identifier for each segment in the storage client
type uploadSegmentID struct {
	// Unique to each dx file
	fid dxfile.FileID

	// Index of each segment within a file
	index uint64
}

// unfinishedUploadSegment represents a segment from the dx filesystem that has not
// finished uploading, including flags of the upload progress
type unfinishedUploadSegment struct {
	// Information about the file. localPath may be the empty string if the file
	// is known not to exist locally.
	id        uploadSegmentID
	fileEntry *dxfile.FileSetEntryWithID
	threadUID int

	// Information about the segment within the file
	// 	+ index: 	array index within dxfile that represents local file in memory
	// 	+ offset: 	actual location in byte unit offset of the segment within the file
	// 	+ length: 	segment length in byte unit within local dx file
	// And these fields can help us to read from dx file
	index  uint64
	offset int64
	length uint64

	memoryNeeded   uint64 // memory needed in bytes
	memoryReleased uint64 // memory that has been returned of memoryNeeded

	sectorsMinNeedNum int // number of sectors minimum to recover file
	sectorsAllNeedNum int // number of sectors of minimum + redundant

	stuck       bool // flag whether the segment was stuck during upload
	stuckRepair bool // flag if the segment was set 'true' for repair by the stuck loop

	// The logical data is the data read from file of user
	// The physical data is all the sectors encrypted and stored on disk across the network
	logicalSegmentData  [][]byte
	physicalSegmentData [][]byte

	mu                  sync.Mutex
	sectorSlotsStatus   []bool              // 'true' in that index if a sector is either uploaded, or a worker is attempting to upload that sector
	sectorsCompletedNum int                 // number of sectors that have been successful completely uploaded
	sectorsUploadingNum int                 // number of sectors that are being uploaded, but aren't finished yet (may fail)
	released            bool                // whether this segment has been released from the active segments set
	unusedHosts         map[string]struct{} // hosts that aren't yet storing any sectors or performing any work
	workersRemain       int                 // number of inactive workers still able to upload a sector
	workerBackups       []*worker           // workers that can be used if other workers fail
}

// notifyBackupWorkers is called when a worker fails to upload a sector, meaning
// that the backup workers may now be needed to help the sector finish uploading
func (uc *unfinishedUploadSegment) notifyBackupWorkers() {
	// Copy the standby workers into a new slice and reset it since we can't
	// hold the lock while calling the managed function.
	uc.mu.Lock()
	backupWorkers := make([]*worker, len(uc.workerBackups))
	copy(backupWorkers, uc.workerBackups)
	uc.workerBackups = uc.workerBackups[:0]
	uc.mu.Unlock()

	randomAssignSectorTaskToWorker(backupWorkers, uc)

	for i := 0; i < len(backupWorkers); i++ {
		backupWorkers[i].signalUploadChan(uc)
	}
}

// IsSegmentUploadComplete checks some fields of the segment to determine if the segment is completed
// 1）no remain workers and no uploading task
// 2) completely upload and no uploading task
func (uc *unfinishedUploadSegment) IsSegmentUploadComplete() bool {
	if uc.sectorsCompletedNum == uc.sectorsAllNeedNum && uc.sectorsUploadingNum == 0 {
		return true
	}

	// We are no longer doing any uploads and we don't have any workers left
	if uc.workersRemain == 0 && uc.sectorsUploadingNum == 0 {
		return true
	}
	return false
}

// dispatchSegment dispatches segments to the workers randomly in the pool in the current solution
// Now it may be that one sector will not be assigned to worker, and this doesn't have a big impact on the upload process
// But we will optimize this features and schedule strategy is more balanced and fair
func (sc *StorageClient) dispatchSegment(uc *unfinishedUploadSegment) {
	// Add segment to pendingSegments map
	sc.uploadHeap.mu.Lock()
	_, exists := sc.uploadHeap.pendingSegments[uc.id]
	if !exists {
		sc.uploadHeap.pendingSegments[uc.id] = struct{}{}
	}
	sc.uploadHeap.mu.Unlock()

	// Distribute the segment to each worker in the work pool, marking the number of workers that have received the segment
	sc.lock.Lock()
	uc.workersRemain += len(sc.workerPool)
	workers := make([]*worker, 0, len(sc.workerPool))
	for _, worker := range sc.workerPool {
		workers = append(workers, worker)
	}
	randomAssignSectorTaskToWorker(workers, uc)
	sc.lock.Unlock()

	for _, worker := range workers {
		worker.signalUploadChan(uc)
	}
}

// randomAssignSectorTaskToWorker will assign randomly non uploaded sector to worker
func randomAssignSectorTaskToWorker(workers []*worker, uc *unfinishedUploadSegment) {
	length := len(workers)
	for i, s := range uc.sectorSlotsStatus {
		workerIndex := (i + rand.Int()) % length
		if !s && workers[workerIndex].isReady(uc) {
			if indexes, ok := workers[workerIndex].sectorIndexMap[uc]; ok {
				indexes = append(indexes, i)
				workers[workerIndex].sectorIndexMap[uc] = indexes
			} else {
				var idx []int
				idx = append(idx, i)
				workers[workerIndex].sectorIndexMap[uc] = idx
			}
			// mark sector usage as true
			uc.sectorSlotsStatus[i] = true
		}
	}
}

// downloadLogicalSegmentData will fetch the logical segment data by sending a
// download to the storage client's downloader, and then assign the data to the field
func (sc *StorageClient) downloadLogicalSegmentData(segment *unfinishedUploadSegment) error {
	downloadLength := segment.length
	if segment.index == uint64(segment.fileEntry.NumSegments()-1) && segment.fileEntry.FileSize()%segment.length != 0 {
		downloadLength = segment.fileEntry.FileSize() % segment.length
	}

	// Create the download
	buf := NewDownloadBuffer(segment.length, segment.fileEntry.SectorSize())
	d, err := sc.newDownload(downloadParams{
		destination:     buf,
		destinationType: "buffer",
		file:            segment.fileEntry.DxFile.Snapshot(),

		latencyTarget: 200e3, // No need to rush latency on repair downloads.
		length:        downloadLength,
		needsMemory:   false, // We already requested memory, the download memory fits inside of that.
		offset:        uint64(segment.offset),
		overdrive:     0, // No need to rush the latency on repair downloads.
		priority:      0, // Repair downloads are completely de-prioritized.
	})
	if err != nil {
		return err
	}

	// Register some cleanup for when the download is done.
	d.onComplete(func(_ error) error {
		// Update the access time when the download is done
		return segment.fileEntry.DxFile.SetTimeAccess(time.Now())
	})

	// Set the in-memory buffer to nil just to be safe in case of a memory leak.
	defer func() {
		d.destination = nil
	}()

	// Wait for the download to complete.
	select {
	case <-d.completeChan:
	case <-sc.tm.StopChan():
		return errors.New("repair download interrupted by stop call")
	}
	if d.Err() != nil {
		buf.buf = nil
		return d.Err()
	}
	segment.logicalSegmentData = [][]byte(buf.buf)
	return nil
}

// retrieveDataAndDispatchSegment will fetch the logical data for a segment, encode
// the physical data for the segment, and then distribute them.
func (sc *StorageClient) retrieveDataAndDispatchSegment(segment *unfinishedUploadSegment) {
	err := sc.tm.Add()
	if err != nil {
		return
	}
	defer sc.tm.Done()

	erasureCodingMemory := segment.fileEntry.SectorSize() * uint64(segment.fileEntry.ErasureCode().MinSectors())
	var sectorCompletedMemory uint64
	for i := 0; i < len(segment.sectorSlotsStatus); i++ {
		if segment.sectorSlotsStatus[i] {
			sectorCompletedMemory += storage.SectorSize
		}
	}

	defer sc.cleanupUploadSegment(segment)

	// Retrieve the logical data for the segment
	err = sc.retrieveLogicalSegmentData(segment)
	if err != nil {
		// retrieve logical data failed, interrupt upload and release memory
		segment.logicalSegmentData = nil
		segment.workersRemain = 0
		sc.memoryManager.Return(erasureCodingMemory + sectorCompletedMemory)
		segment.memoryReleased += erasureCodingMemory + sectorCompletedMemory
		sc.log.Debug("retrieve logical data of a segment failed:", err)
		return
	}

	// Encode the physical sectors from content bytes of file
	segmentBytes := make([]byte, uint64(len(segment.logicalSegmentData))*storage.SectorSize)
	for _, b := range segment.logicalSegmentData {
		segmentBytes = append(segmentBytes, b...)
	}
	segment.physicalSegmentData, err = segment.fileEntry.ErasureCode().Encode(segmentBytes)
	if err != nil {
		segment.workersRemain = 0
		sc.memoryManager.Return(sectorCompletedMemory)
		segment.memoryReleased += sectorCompletedMemory
		for i := 0; i < len(segment.physicalSegmentData); i++ {
			segment.physicalSegmentData[i] = nil
		}
		sc.log.Debug("Erasure encode physical data of a segment failed:", err)
		return
	}

	segment.logicalSegmentData = nil
	sc.memoryManager.Return(erasureCodingMemory)
	segment.memoryReleased += erasureCodingMemory

	// Sanity check that at least as many physical data sectors as sector slots
	if len(segment.physicalSegmentData) < len(segment.sectorSlotsStatus) {
		sc.log.Error("not enough physical sectors to match the upload sector slots of the file")
		return
	}

	// Loop through the sectorSlots and encrypt any that are needed
	// If the sector has been used, set physicalSegmentData nil and gc routine will collect this memory
	for i := 0; i < len(segment.sectorSlotsStatus); i++ {
		if segment.sectorSlotsStatus[i] {
			segment.physicalSegmentData[i] = nil
		} else {
			cipherData, err := segment.fileEntry.CipherKey().Encrypt(segment.physicalSegmentData[i])
			// TODO Discuss 加密失败之后，是传明文还是忽略该segment
			if err != nil {
				//segment.physicalSegmentData[i] = nil
				sc.log.Debug("encrypt segment after erasure encode failed: ", err)
			} else {
				segment.physicalSegmentData[i] = cipherData
			}

		}
	}

	if sectorCompletedMemory > 0 {
		sc.memoryManager.Return(sectorCompletedMemory)
		segment.memoryReleased += sectorCompletedMemory
	}

	sc.dispatchSegment(segment)
}

// retrieveLogicalSegmentData will get the raw data from disk if possible otherwise queueing a download
func (sc *StorageClient) retrieveLogicalSegmentData(segment *unfinishedUploadSegment) error {
	numRedundantSectors := float64(segment.sectorsAllNeedNum - segment.sectorsMinNeedNum)
	minMissingSectorsToDownload := int(numRedundantSectors * RemoteRepairDownloadThreshold)
	needDownload := segment.sectorsCompletedNum+minMissingSectorsToDownload < segment.sectorsAllNeedNum

	// Download the segment if it's not on disk.
	if segment.fileEntry.LocalPath() == "" && needDownload {
		return sc.downloadLogicalSegmentData(segment)
	} else if segment.fileEntry.LocalPath() == "" {
		return errors.New("file not available locally")
	}

	// Try to read the file content from disk. If failed, go through needDownload
	osFile, err := os.Open(string(segment.fileEntry.LocalPath()))
	if err != nil && needDownload {
		return sc.downloadLogicalSegmentData(segment)
	} else if err != nil {
		return errors.New("failed to open file locally")
	}
	defer osFile.Close()

	buf := NewDownloadBuffer(segment.length, segment.fileEntry.SectorSize())
	sr := io.NewSectionReader(osFile, segment.offset, int64(segment.length))
	_, err = buf.ReadFrom(sr)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF && needDownload {
		sc.log.Debug("failed to read file, downloading instead:", err)
		return sc.downloadLogicalSegmentData(segment)
	} else if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		sc.log.Debug("failed to read file locally:", err)
		return errors.New("failed to read file locally")
	}
	segment.logicalSegmentData = buf.buf

	return nil
}

// cleanupUploadSegment will check the state of the segment and perform any
// cleanup required. This can include returning memory and releasing the segment
// from the map of active segments in the segment heap.
func (sc *StorageClient) cleanupUploadSegment(uc *unfinishedUploadSegment) {
	uc.mu.Lock()
	sectorsAvailable := 0
	var memoryReleased uint64
	// Release any unnecessary pieces, counting any pieces that are
	// currently available.
	for i := 0; i < len(uc.sectorSlotsStatus); i++ {
		// Skip the piece if it's not available.
		if uc.sectorSlotsStatus[i] {
			continue
		}

		// If we have all the available sectors we need, release this sector.
		// Otherwise, mark that there's another sector available. This algorithm
		// will prefer releasing later sectors, which improves computational
		// complexity for erasure coding
		if sectorsAvailable >= uc.workersRemain {
			memoryReleased += storage.SectorSize
			if len(uc.physicalSegmentData) < len(uc.sectorSlotsStatus) {
				// TODO handle this. Might happen if erasure coding the Segment failed.
			}
			uc.physicalSegmentData[i] = nil
			// Mark this sector as taken so that we don't double release memory
			uc.sectorSlotsStatus[i] = true
		} else {
			sectorsAvailable++
		}
	}

	// Check if the Segment needs to be removed from the list of active
	// segments. It needs to be removed if the segment is complete, but hasn't
	// yet been released
	segmentComplete := uc.IsSegmentUploadComplete()
	released := uc.released
	if segmentComplete && !released {
		uc.released = true
	}
	uc.memoryReleased += uint64(memoryReleased)
	totalMemoryReleased := uc.memoryReleased
	uc.mu.Unlock()

	// If there are pieces available, add the standby workers to collect them.
	// Standby workers are only added to the Segment when sectorsAvailable is equal
	// to zero, meaning this code will only trigger if the number of pieces
	// available increases from zero. That can only happen if a worker
	// experiences an error during upload.
	if sectorsAvailable > 0 {
		uc.notifyBackupWorkers()
	}
	// If required, return the memory to the storage client.
	if memoryReleased > 0 {
		sc.memoryManager.Return(memoryReleased)
	}
	// If required, remove the segment from the set of repairing segments.
	if segmentComplete && !released {
		sc.updateUploadSegmentStuckStatus(uc)
		err := uc.fileEntry.Close()
		if err != nil {
			sc.log.Debug("file not closed after segment upload complete: %v %v", uc.fileEntry.DxPath(), err)
		}
		sc.uploadHeap.mu.Lock()
		delete(sc.uploadHeap.pendingSegments, uc.id)
		sc.uploadHeap.mu.Unlock()
	}
	// Sanity check - all memory should be released if the Segment is complete.
	if segmentComplete && totalMemoryReleased != uc.memoryNeeded {
		sc.log.Debug("No workers remaining, but not all memory released:", uc.workersRemain, uc.sectorsUploadingNum, uc.memoryReleased, uc.memoryNeeded)
	}
}

// setStuckAndClose sets the unfinishedUploadSegment's stuck status
func (sc *StorageClient) setStuckAndClose(uc *unfinishedUploadSegment, stuck bool) error {
	err := uc.fileEntry.SetStuckByIndex(int(uc.index), stuck)
	if err != nil {
		return fmt.Errorf("unable to update Segment stuck status for file %v: %v", uc.fileEntry.DxPath(), err)
	}

	go sc.fileSystem.InitAndUpdateDirMetadata(uc.fileEntry.DxPath())

	err = uc.fileEntry.Close()
	if err != nil {
		return fmt.Errorf("unable to close dx file %v", uc.fileEntry.DxPath())
	}
	return nil
}

// updateUploadSegmentStuckStatus checks to see if the repair was
// successful and then updates the segment's stuck status
func (sc *StorageClient) updateUploadSegmentStuckStatus(uc *unfinishedUploadSegment) {
	// Grab necessary information from upload Segment under lock
	uc.mu.Lock()
	index := uc.id.index
	stuck := uc.stuck
	sectorsCompleteNum := uc.sectorsCompletedNum
	sectorsNeedNum := uc.sectorsAllNeedNum
	stuckRepair := uc.stuckRepair
	uc.mu.Unlock()

	// Determine if repair was successful
	successfulRepair := (1-RemoteRepairDownloadThreshold)*float64(sectorsNeedNum) <= float64(sectorsCompleteNum)

	// Check if client shut down
	var clientOffline bool
	select {
	case <-sc.tm.StopChan():
		clientOffline = true
	default:
		// Check that the storage client is still online
		if !sc.Online() {
			clientOffline = true
		}
	}

	// If the repair was unsuccessful and there was a client closed then return
	if !successfulRepair && clientOffline {
		sc.log.Debug("repair unsuccessful for Segment", uc.id, "due to client shut down")
		return
	}
	// Log if the repair was unsuccessful
	if !successfulRepair {
		sc.log.Debug("repair unsuccessful, marking segment", uc.id, "as stuck", float64(sectorsCompleteNum)/float64(sectorsNeedNum))
	} else {
		sc.log.Debug("SUCCESS: repair successsful, marking segment as non-stuck:", uc.id)
	}

	if err := uc.fileEntry.SetStuckByIndex(int(index), !successfulRepair); err != nil {
		sc.log.Debug("could not set segment %v stuck status for file %v: %v", uc.id, uc.fileEntry.DxPath(), err)
	}

	dxPath := uc.fileEntry.DxPath()

	if err := sc.fileSystem.InitAndUpdateDirMetadata(dxPath); err != nil {
		sc.log.Error("update dir meta data failed: ", err)
	}

	// Check to see if the segment was stuck and now is successfully repaired by the stuck loop
	if stuck && successfulRepair && stuckRepair {
		// Signal the stuck loop that the Segment was successfully repaired
		sc.log.Debug("Stuck segment", uc.id, "successfully repaired")
		select {
		case <-sc.tm.StopChan():
			sc.log.Debug("storage client shut down before the stuck loop was signalled that the stuck repair was successful")
			return
		case sc.uploadHeap.stuckSegmentSuccess <- dxPath:
		}
	}
}
