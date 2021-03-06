package internal

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/jackc/pgx"
	"github.com/wal-g/storages/storage"
	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/utility"
)

type WalStorageStatus int

const (
	// No missing segments in storage
	Ok WalStorageStatus = iota + 1
	// Storage contains some ProbablyUploading or ProbablyDelayed missing segments
	Warning
	// Storage contains lost missing segments
	Failure
)

func (storageStatus WalStorageStatus) String() string {
	return [...]string{"", "OK", "WARNING", "FAILURE"}[storageStatus]
}

// MarshalJSON marshals the WalStorageStatus enum as a quoted json string
func (storageStatus WalStorageStatus) MarshalJSON() ([]byte, error) {
	return marshalEnumToJSON(storageStatus)
}

type WalVerifyResult struct {
	StorageStatus       WalStorageStatus                   `json:"storage_status"`
	IntegrityScanResult []*WalIntegrityScanSegmentSequence `json:"integrity_scan_result"`
}

func newWalVerifyResult(integrityScanResult []*WalIntegrityScanSegmentSequence) WalVerifyResult {
	walVerifyResult := WalVerifyResult{StorageStatus: Ok, IntegrityScanResult: integrityScanResult}
	for _, row := range integrityScanResult {
		switch row.Status {
		case Lost:
			walVerifyResult.StorageStatus = Failure
			return walVerifyResult
		case ProbablyUploading:
		case ProbablyDelayed:
			walVerifyResult.StorageStatus = Warning
		}
	}
	return walVerifyResult
}

// WalIntegrityScanSegmentSequence is a continuous sequence of segments
// with the same timeline and status
type WalIntegrityScanSegmentSequence struct {
	TimelineId    uint32               `json:"timeline_id"`
	StartSegment  string               `json:"start_segment"`
	EndSegment    string               `json:"end_segment"`
	SegmentsCount int                  `json:"segments_count"`
	Status        ScannedSegmentStatus `json:"status"`
}

func newWalIntegrityScanSegmentSequence(sequence *WalSegmentsSequence,
	status ScannedSegmentStatus) *WalIntegrityScanSegmentSequence {
	return &WalIntegrityScanSegmentSequence{
		TimelineId:    sequence.timelineId,
		StartSegment:  sequence.minSegmentNo.getFilename(sequence.timelineId),
		EndSegment:    sequence.maxSegmentNo.getFilename(sequence.timelineId),
		Status:        status,
		SegmentsCount: len(sequence.walSegmentNumbers),
	}
}

// QueryCurrentWalSegment() gets start WAL segment from Postgres cluster
func QueryCurrentWalSegment() WalSegmentDescription {
	conn, err := Connect()
	tracelog.ErrorLogger.FatalfOnError("Failed to establish a connection to Postgres cluster %v", err)

	queryRunner, err := newPgQueryRunner(conn)
	tracelog.ErrorLogger.FatalfOnError("Failed to initialize PgQueryRunner %v", err)

	currentSegmentNo, err := getCurrentWalSegmentNo(queryRunner)
	tracelog.ErrorLogger.FatalfOnError("Failed to get current WAL segment number %v", err)

	currentTimeline, err := getCurrentTimeline(conn)
	tracelog.ErrorLogger.FatalfOnError("Failed to get current timeline %v", err)

	err = conn.Close()
	tracelog.WarningLogger.PrintOnError(err)

	// currentSegment is the current WAL segment of the cluster
	return WalSegmentDescription{Timeline: currentTimeline, Number: currentSegmentNo}
}

// HandleWalVerify queries the current cluster WAL segment and timeline
// and travels through WAL segments in storage in reversed chronological order (starting from that segment)
// to find any missing WAL segments that could potentially fail the PITR procedure
func HandleWalVerify(rootFolder storage.Folder, startWalSegment WalSegmentDescription, outputWriter WalVerifyOutputWriter) {
	walFolder := rootFolder.GetSubFolder(utility.WalPath)
	storageFileNames, err := getFolderFilenames(walFolder)
	tracelog.ErrorLogger.FatalfOnError("Failed to get WAL folder filenames %v", err)

	storageSegments := getSegmentsFromFiles(storageFileNames)
	timelineSwitchMap, err := createTimelineSwitchMap(startWalSegment.Timeline, walFolder)
	tracelog.ErrorLogger.FatalfOnError("Failed to initialize timeline history map %v", err)
	walSegmentRunner := NewWalSegmentRunner(startWalSegment, storageSegments, 1, timelineSwitchMap)

	// maxConcurrency is needed to determine max amount of missing WAL segments
	// after the last found WAL segment which can be skipped ("uploading" segment sequence size)
	maxConcurrency, err := getMaxUploadConcurrency()
	tracelog.ErrorLogger.FatalOnError(err)

	segmentScanner := NewWalSegmentScanner(walSegmentRunner)
	err = runWalIntegrityScan(segmentScanner, maxConcurrency)
	tracelog.ErrorLogger.FatalfOnError("Failed to perform WAL segments scan %v", err)

	integrityScanSegmentSequences, err := collapseSegmentsByStatusAndTimeline(segmentScanner.ScannedSegments)
	tracelog.ErrorLogger.FatalOnError(err)

	err = outputWriter.Write(newWalVerifyResult(integrityScanSegmentSequences))
	tracelog.ErrorLogger.FatalOnError(err)
}

// runWalIntegrityScan invokes the following storage scan series
// (on each iteration scanner continues from the position where it stopped)
// 1. At first, it runs scan until it finds some segment in WAL storage
// and marks all encountered missing segments as "missing, probably delayed"
// 2. Then it scans exactly uploadingSegmentRangeSize count of segments,
// if found any missing segments it marks them as "missing, probably still uploading"
// 3. Final scan without any limit (until stopSegment is reached),
// all missing segments encountered in this scan are considered as "missing, lost"
func runWalIntegrityScan(scanner *WalSegmentScanner, uploadingSegmentRangeSize int) error {
	// Run to the latest WAL segment available in storage, mark all missing segments as delayed
	err := scanner.Scan(SegmentScanConfig{
		UnlimitedScan:           true,
		StopOnFirstFoundSegment: true,
		MissingSegmentStatus:    ProbablyDelayed,
	})
	if err != nil {
		return err
	}

	// Traverse potentially uploading segments, mark all missing segments as probably uploading
	err = scanner.Scan(SegmentScanConfig{
		ScanSegmentsLimit:    uploadingSegmentRangeSize,
		MissingSegmentStatus: ProbablyUploading,
	})
	if err != nil {
		return err
	}

	// Run until stop segment, and mark all missing segments as lost
	return scanner.Scan(SegmentScanConfig{
		UnlimitedScan:        true,
		MissingSegmentStatus: Lost,
	})
}

// collapseSegmentsByStatusAndTimeline collapses scanned segments
// with the same timeline and status into segment sequences to minify the output
func collapseSegmentsByStatusAndTimeline(scannedSegments []ScannedSegmentDescription) ([]*WalIntegrityScanSegmentSequence, error) {
	if len(scannedSegments) == 0 {
		return nil, nil
	}

	// make sure that ScannedSegments are ordered
	sort.Slice(scannedSegments, func(i, j int) bool {
		return scannedSegments[i].Number < scannedSegments[j].Number
	})

	segmentSequences := make([]*WalIntegrityScanSegmentSequence, 0)
	currentSequence := NewSegmentsSequence(scannedSegments[0].Timeline, scannedSegments[0].Number)
	currentStatus := scannedSegments[0].status

	for i := 1; i < len(scannedSegments); i++ {
		segment := scannedSegments[i]

		// switch to the new sequence on segment Status change or timeline id change
		if segment.status != currentStatus || currentSequence.timelineId != segment.Timeline {
			segmentSequences = append(segmentSequences, newWalIntegrityScanSegmentSequence(currentSequence, currentStatus))
			currentSequence = NewSegmentsSequence(segment.Timeline, segment.Number)
			currentStatus = segment.status
		} else {
			currentSequence.AddWalSegmentNo(segment.Number)
		}
	}

	segmentSequences = append(segmentSequences, newWalIntegrityScanSegmentSequence(currentSequence, currentStatus))
	return segmentSequences, nil
}

// get the current wal segment number of the cluster
func getCurrentWalSegmentNo(queryRunner *PgQueryRunner) (WalSegmentNo, error) {
	lsnStr, err := queryRunner.getCurrentLsn()
	if err != nil {
		return 0, err
	}
	lsn, err := pgx.ParseLSN(lsnStr)
	if err != nil {
		return 0, err
	}
	return newWalSegmentNo(lsn - 1), nil
}

// get the current timeline of the cluster
func getCurrentTimeline(conn *pgx.Conn) (uint32, error) {
	timeline, err := readTimeline(conn)
	if err != nil {
		return 0, err
	}
	return timeline, nil
}

// marshalEnumToJSON is used to write the string enum representation
// instead of int enum value to JSON
func marshalEnumToJSON(enum fmt.Stringer) ([]byte, error) {
	buffer := bytes.NewBufferString(fmt.Sprintf(`"%s"`, enum))
	return buffer.Bytes(), nil
}
