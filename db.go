package litefs

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/superfly/litefs/internal"
	"github.com/superfly/ltx"
)

// DBMode represents either a rollback journal or WAL mode.
type DBMode int

// Database journal modes.
const (
	DBModeRollback = DBMode(0)
	DBModeWAL      = DBMode(1)
)

// DB represents a SQLite database.
type DB struct {
	mu       sync.Mutex
	store    *Store // parent store
	name     string // name of database
	path     string // full on-disk path
	pageSize uint32 // database page size, if known
	pageN    uint32 // database size, in pages
	pos      Pos    // current tx position
	mode     DBMode // database journaling mode (rollback, wal)

	dirtyPageSet map[uint32]struct{}

	walOffset       int64            // offset of the start of the transaction
	walFrameOffsets map[uint32]int64 // WAL frame offset of the last version of a given pgno before current tx

	// SQLite database locks
	pendingLock  RWMutex
	sharedLock   RWMutex
	reservedLock RWMutex

	// SQLite WAL locks
	writeLock   RWMutex
	ckptLock    RWMutex
	recoverLock RWMutex
	read0Lock   RWMutex
	read1Lock   RWMutex
	read2Lock   RWMutex
	read3Lock   RWMutex
	read4Lock   RWMutex
	dmsLock     RWMutex

	// Returns the current time. Used for mocking time in tests.
	Now func() time.Time
}

// NewDB returns a new instance of DB.
func NewDB(store *Store, name string, path string) *DB {
	return &DB{
		store: store,
		name:  name,
		path:  path,

		dirtyPageSet:    make(map[uint32]struct{}),
		walFrameOffsets: make(map[uint32]int64),

		Now: time.Now,
	}
}

// Name of the database name.
func (db *DB) Name() string { return db.name }

// Store returns the store that the database is a member of.
func (db *DB) Store() *Store { return db.store }

// Path of the database's data directory.
func (db *DB) Path() string { return db.path }

// LTXDir returns the path to the directory of LTX transaction files.
func (db *DB) LTXDir() string { return filepath.Join(db.path, "ltx") }

// LTXPath returns the path of an LTX file.
func (db *DB) LTXPath(minTXID, maxTXID uint64) string {
	return filepath.Join(db.LTXDir(), ltx.FormatFilename(minTXID, maxTXID))
}

// ReadLTXDir returns DirEntry for every LTX file.
func (db *DB) ReadLTXDir() ([]fs.DirEntry, error) {
	ents, err := os.ReadDir(db.LTXDir())
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("readdir: %w", err)
	}

	for i := 0; i < len(ents); i++ {
		if _, _, err := ltx.ParseFilename(ents[i].Name()); err != nil {
			ents, i = append(ents[:i], ents[i+1:]...), i-1
		}
	}

	// Ensure results are in sorted order.
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })

	return ents, nil
}

// DatabasePath returns the path to the underlying database file.
func (db *DB) DatabasePath() string { return filepath.Join(db.path, "database") }

// JournalPath returns the path to the underlying journal file.
func (db *DB) JournalPath() string { return filepath.Join(db.path, "journal") }

// WALPath returns the path to the underlying WAL file.
func (db *DB) WALPath() string { return filepath.Join(db.path, "wal") }

// SHMPath returns the path to the underlying shared memory file.
func (db *DB) SHMPath() string { return filepath.Join(db.path, "shm") }

// PageSize returns the page size of the underlying database.
func (db *DB) PageSize() uint32 {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.pageSize
}

// Pos returns the current transaction position of the database.
func (db *DB) Pos() Pos {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.pos
}

// setPos sets the current transaction position of the database.
func (db *DB) setPos(pos Pos) error {
	db.pos = pos

	// Invalidate page cache.
	if invalidator := db.store.Invalidator; invalidator != nil {
		if err := invalidator.InvalidatePos(db); err != nil {
			return fmt.Errorf("invalidate pos: %w", err)
		}
	}

	// Update metrics.
	dbTXIDMetricVec.WithLabelValues(db.name).Set(float64(db.pos.TXID))

	return nil
}

// TXID returns the current transaction ID.
func (db *DB) TXID() uint64 { return db.Pos().TXID }

// Open initializes the database from files in its data directory.
func (db *DB) Open() error {
	// Ensure "ltx" directory exists.
	if err := os.MkdirAll(db.LTXDir(), 0777); err != nil {
		return err
	}

	// Read page size & page count from database file.
	if err := db.initFromDatabaseHeader(); err != nil {
		return fmt.Errorf("init from database header: %w", err)
	}

	// Remove all SHM files on start up.
	if err := os.Remove(db.SHMPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove shm: %w", err)
	}

	// If a journal file exists, rollback the last transaction so that we
	// are in a consistent state before applying our last LTX file. Otherwise
	// we could have a partial transaction in our database, apply our LTX, and
	// then the SQLite client will recover from the journal and corrupt everything.
	//
	// See: https://github.com/superfly/litefs/issues/134
	if err := db.rollbackJournal(context.Background()); err != nil {
		return fmt.Errorf("rollback journal: %w", err)
	}

	// Copy the WAL file back to the main database. This ensures that we can
	// compute the checksum only using the database file instead of having to
	// first compute the latest page set from the WAL to overlay.
	if err := db.checkpoint(context.Background()); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}

	if err := db.recoverFromLTX(); err != nil {
		return fmt.Errorf("recover ltx: %w", err)
	}

	// Validate database file.
	if err := db.verifyDatabaseFile(); err != nil {
		return fmt.Errorf("verify database file: %w", err)
	}

	return nil
}

// initFromDatabaseHeader reads the page size & page count from the database file header.
func (db *DB) initFromDatabaseHeader() error {
	f, err := os.Open(db.DatabasePath())
	if os.IsNotExist(err) {
		return nil // no database file yet, skip
	} else if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// Read page size into memory.
	hdr, err := readSQLiteDatabaseHeader(f)
	if err == io.EOF {
		return nil // empty file, skip
	} else if err != nil {
		return err
	}
	db.pageSize = hdr.PageSize
	db.pageN = hdr.PageN

	// Initialize database mode.
	if hdr.WriteVersion == 2 && hdr.ReadVersion == 2 {
		db.mode = DBModeWAL
	} else {
		db.mode = DBModeRollback
	}

	return nil
}

// rollbackJournal copies all the pages from an existing rollback journal back
// to the database file. This is called on startup so that we can be in a
// consistent state in order to verify our checksums.
func (db *DB) rollbackJournal(ctx context.Context) error {
	journalFile, err := os.OpenFile(db.JournalPath(), os.O_RDWR, 0666)
	if os.IsNotExist(err) {
		return nil // no journal file, skip
	} else if err != nil {
		return err
	}
	defer func() { _ = journalFile.Close() }()

	dbFile, err := os.OpenFile(db.DatabasePath(), os.O_RDWR, 0666)
	if err != nil {
		return err
	}
	defer func() { _ = dbFile.Close() }()

	// Copy every journal page back into the main database file.
	r := NewJournalReader(journalFile)
	for i := 0; ; i++ {
		if err := r.Next(); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("next segment(%d): %w", i, err)
		}
		if err := db.rollbackJournalSegment(ctx, r, dbFile); err != nil {
			return fmt.Errorf("segment(%d): %w", i, err)
		}
	}

	// Resize database to size before journal transaction.
	if err := dbFile.Truncate(r.DatabaseSize()); err != nil {
		return err
	}

	if err := dbFile.Sync(); err != nil {
		return err
	} else if err := dbFile.Close(); err != nil {
		return err
	} else if err := journalFile.Close(); err != nil {
		return err
	}

	return nil
}

func (db *DB) rollbackJournalSegment(ctx context.Context, r *JournalReader, dbFile *os.File) error {
	for i := 0; ; i++ {
		pgno, data, err := r.ReadFrame()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("read frame(%d): %w", i, err)
		}

		// Write data to the database file.
		if _, err := dbFile.WriteAt(data, int64(pgno-1)*int64(db.pageSize)); err != nil {
			return fmt.Errorf("write to database (pgno=%d): %w", pgno, err)
		}
	}
}

func (db *DB) checkpoint(ctx context.Context) error {
	// Open the database file we'll checkpoint into. Skip if this hasn't been created.
	dbFile, err := os.OpenFile(db.DatabasePath(), os.O_RDWR, 0666)
	if os.IsNotExist(err) {
		return nil // no database file yet, skip
	} else if err != nil {
		return err
	}
	defer func() { _ = dbFile.Close() }()

	// Open the WAL file that we'll copy from. Skip if it was cleanly closed and removed.
	walFile, err := os.Open(db.WALPath())
	if os.IsNotExist(err) {
		return nil // no WAL file, skip
	} else if err != nil {
		return err
	}
	defer func() { _ = walFile.Close() }()

	offsets, commit, err := db.readWALPageOffsets(walFile)
	if err != nil {
		return fmt.Errorf("read wal page offsets: %w", err)
	}

	// Copy pages from the WAL to the main database file & resize db file.
	if len(offsets) > 0 {
		buf := make([]byte, db.pageSize)
		for pgno, offset := range offsets {
			if _, err := walFile.Seek(offset+WALFrameHeaderSize, io.SeekStart); err != nil {
				return fmt.Errorf("seek wal: %w", err)
			} else if _, err := io.ReadFull(walFile, buf); err != nil {
				return fmt.Errorf("read wal: %w", err)
			}

			if _, err := dbFile.WriteAt(buf, int64(pgno-1)*int64(db.pageSize)); err != nil {
				return fmt.Errorf("write db page %d: %w", pgno, err)
			}
		}

		if err := dbFile.Truncate(int64(commit) * int64(db.pageSize)); err != nil {
			return fmt.Errorf("truncate: %w", err)
		}

		// Save the size of the database, in pages, based on last commit.
		db.pageN = commit
	}

	// Remove WAL file.
	if err := os.Remove(db.WALPath()); err != nil {
		return fmt.Errorf("remove wal: %w", err)
	}

	return nil
}

// readWALPageOffsets returns a map of the offsets of the last committed version
// of each page in the WAL. Also returns the commit size of the last transaction.
func (db *DB) readWALPageOffsets(f *os.File) (_ map[uint32]int64, lastCommit uint32, _ error) {
	r := NewWALReader(f)
	if err := r.ReadHeader(); err == io.EOF {
		return nil, 0, nil
	}

	// Read the offset of the last version of each page in the WAL.
	offsets := make(map[uint32]int64)
	txOffsets := make(map[uint32]int64)
	buf := make([]byte, r.PageSize())
	for {
		pgno, commit, err := r.ReadFrame(buf)
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, 0, err
		}

		// Save latest offset for each page version.
		txOffsets[pgno] = r.Offset()

		// If this is not a committing frame, continue to next frame.
		if commit == 0 {
			continue
		}

		// At the end of each transaction, copy offsets to main map.
		lastCommit = commit
		for k, v := range txOffsets {
			offsets[k] = v
		}
		txOffsets = make(map[uint32]int64)
	}

	return offsets, lastCommit, nil
}

// recoverFromLTX finds the last LTX file to determine the TXID and then
// re-applies the last file in case transaction was lost from the journal/WAL.
func (db *DB) recoverFromLTX() error {
	fis, err := os.ReadDir(db.LTXDir())
	if err != nil {
		return fmt.Errorf("readdir: %w", err)
	}

	var pos Pos
	var filename string
	for _, fi := range fis {
		minTXID, maxTXID, err := ltx.ParseFilename(fi.Name())
		if err != nil {
			continue
		}

		// Read header to find the checksum for the transaction.
		header, trailer, err := readAndVerifyLTXFile(filepath.Join(db.LTXDir(), fi.Name()))
		if err != nil {
			return fmt.Errorf("read ltx file header (%s): %w", fi.Name(), err)
		}

		// Ensure header TXIDs match the filename.
		if header.MinTXID != minTXID || header.MaxTXID != maxTXID {
			return fmt.Errorf("ltx header txid (%s,%s) does not match filename (%s)", ltx.FormatTXID(header.MaxTXID), ltx.FormatTXID(maxTXID), fi.Name())
		}

		// Save latest position.
		if header.MaxTXID > pos.TXID {
			pos = Pos{
				TXID:              header.MaxTXID,
				PostApplyChecksum: trailer.PostApplyChecksum,
			}
			filename = fi.Name()
		}
	}

	// Ignore remaining processing if we don't have an LTX file to load from.
	if pos.IsZero() {
		return nil
	}

	// Apply the last LTX file so our checksums match if there was a failure in
	// between the LTX commit and journal/WAL commit.
	if err := db.ApplyLTX(context.Background(), filepath.Join(db.LTXDir(), filename)); err != nil {
		return fmt.Errorf("apply ltx: %w", err)
	}

	return nil
}

// verifyDatabaseFile opens and validates the database file, if it exists.
func (db *DB) verifyDatabaseFile() error {
	f, err := os.Open(db.DatabasePath())
	if os.IsNotExist(err) {
		return nil // no database file yet
	} else if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	hdr, err := readSQLiteDatabaseHeader(f)
	if err == io.EOF {
		return nil // no contents yet
	} else if err != nil {
		return fmt.Errorf("cannot read database header: %w", err)
	}
	db.pageSize = hdr.PageSize

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek to start of database: %w", err)
	}

	// Calculate checksum for entire database.
	chksum, err := ltx.ChecksumReader(f, int(db.pageSize))
	if err != nil {
		return fmt.Errorf("checksum database: %w", err)
	}

	// Ensure database checksum matches checksum in current position.
	if chksum != db.pos.PostApplyChecksum {
		return fmt.Errorf("database checksum (%016x) does not match latest LTX checksum (%016x)", chksum, db.pos.PostApplyChecksum)
	}

	return nil
}

// OpenLTXFile returns a file handle to an LTX file that contains the given TXID.
func (db *DB) OpenLTXFile(txID uint64) (*os.File, error) {
	return os.Open(db.LTXPath(txID, txID))
}

// WriteDatabase writes data to the main database file.
func (db *DB) WriteDatabase(f *os.File, data []byte, offset int64) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Return an error if the current process is not the leader.
	if !db.store.IsPrimary() {
		return ErrReadOnlyReplica
	} else if len(data) == 0 {
		return nil
	}

	// Use page size from the write.
	if db.pageSize == 0 {
		if offset != 0 {
			return fmt.Errorf("cannot determine page size, initial offset (%d) is non-zero", offset)
		}
		hdr, err := readSQLiteDatabaseHeader(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("cannot read sqlite database header: %w", err)
		}
		db.pageSize = hdr.PageSize
	}

	// Mark page as dirty.
	pgno := uint32(offset/int64(db.pageSize)) + 1
	db.dirtyPageSet[pgno] = struct{}{}

	// Callback to perform write on handle.
	if _, err := f.WriteAt(data, offset); err != nil {
		return err
	}

	dbDatabaseWriteCountMetricVec.WithLabelValues(db.name).Inc()
	return nil
}

// CreateJournal creates a new journal file on disk.
func (db *DB) CreateJournal() (*os.File, error) {
	if !db.store.IsPrimary() {
		return nil, ErrReadOnlyReplica
	}
	return os.OpenFile(db.JournalPath(), os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_TRUNC, 0666)
}

// CreateWAL creates a new WAL file on disk.
func (db *DB) CreateWAL() (*os.File, error) {
	return os.OpenFile(db.WALPath(), os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_TRUNC, 0666)
}

// WriteWAL writes data to the WAL file. On final commit write, an LTX file is
// generated for the transaction.
func (db *DB) WriteWAL(f *os.File, data []byte, offset int64) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Return an error if the current process is not the leader.
	if !db.store.IsPrimary() {
		return ErrReadOnlyReplica
	} else if len(data) == 0 {
		return nil
	}

	assert(db.pageSize != 0, "page size cannot be zero for wal write")

	dbWALWriteCountMetricVec.WithLabelValues(db.name).Inc()

	// Reset WAL if header is overwritten.
	if offset == 0 {
		db.walOffset = WALHeaderSize
		db.walFrameOffsets = make(map[uint32]int64)
	}

	// Passthrough write to underlying WAL file.
	if _, err := f.WriteAt(data, offset); err != nil {
		return err
	}

	// If this write does not finish at the end of a frame, then exit.
	walFrameSize := int64(WALFrameHeaderSize + db.pageSize)
	endOffset := offset + int64(len(data))
	if endOffset == WALHeaderSize || (endOffset-WALHeaderSize)%walFrameSize != 0 {
		return nil
	}

	// Check if the frame is the commit record.
	fhdr := make([]byte, WALFrameHeaderSize)
	if _, err := f.Seek(endOffset-walFrameSize, io.SeekStart); err != nil {
		return fmt.Errorf("seek wal frame start: %w", err)
	} else if _, err := io.ReadFull(f, fhdr); err != nil {
		return fmt.Errorf("seek wal frame header: %w", err)
	}
	commit := binary.BigEndian.Uint32(fhdr[4:8])
	if commit == 0 {
		return nil // not a commit frame, exit
	}

	if err := db.commitWAL(f, commit); err != nil {
		return fmt.Errorf("commit wal: %w", err)
	}
	return nil
}

func (db *DB) buildTxFrameOffsets(walFile *os.File) (map[uint32]int64, error) {
	m := make(map[uint32]int64)

	if _, err := walFile.Seek(db.walOffset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek wal tx start: %w", err)
	}

	walFrameSize := WALFrameHeaderSize + int64(db.pageSize)
	frame := make([]byte, walFrameSize)
	for i := 0; ; i++ {
		if _, err := io.ReadFull(walFile, frame); err != nil {
			return nil, fmt.Errorf("read next frame: %w", err)
		}
		pgno := binary.BigEndian.Uint32(frame[0:4])
		m[pgno] = db.walOffset + (int64(i) * walFrameSize)

		if commit := binary.BigEndian.Uint32(frame[4:8]); commit != 0 {
			return m, nil // end of tx
		}
	}
}

// commitWAL is called on the last write to the WAL page in a transaction.
// The transaction data is copied from the WAL into an LTX file and committed.
func (db *DB) commitWAL(walFile *os.File, commit uint32) error {
	walFrameSize := int64(WALFrameHeaderSize + db.pageSize)

	// Sync WAL to disk as this avoids data loss issues with SYNCHRONOUS=normal
	if err := walFile.Sync(); err != nil {
		return fmt.Errorf("sync wal: %w", err)
	}

	// Build offset map for the last version of each page in the WAL transaction.
	txFrameOffsets, err := db.buildTxFrameOffsets(walFile)
	if err != nil {
		return fmt.Errorf("build tx frame offsets: %w", err)
	}

	dbFile, err := os.Open(db.DatabasePath())
	if err != nil {
		return fmt.Errorf("cannot open database file: %w", err)
	}
	defer func() { _ = dbFile.Close() }()

	// Read previous page 1 and extract page count.
	page := make([]byte, db.pageSize)
	if err := db.readPage(dbFile, walFile, 1, page); err != nil {
		return fmt.Errorf("read prev page 1: %w", err)
	}
	prevPageN := binary.BigEndian.Uint32(page[SQLITE_DATABASE_SIZE_OFFSET:])

	// Determine transaction ID of the in-process transaction.
	pos := db.pos
	txID := pos.TXID + 1

	// Compute rolling checksum based off previous LTX database checksum.
	preApplyChecksum := pos.PostApplyChecksum
	postApplyChecksum := pos.PostApplyChecksum // start from previous chksum

	// Open file descriptors for the header & page blocks for new LTX file.
	ltxPath := db.LTXPath(txID, txID)
	tmpPath := ltxPath + ".tmp"
	_ = os.Remove(tmpPath)

	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("cannot create LTX file: %w", err)
	}
	defer func() { _ = f.Close() }()

	enc := ltx.NewEncoder(f)
	if err := enc.EncodeHeader(ltx.Header{
		Version:          1,
		PageSize:         db.pageSize,
		Commit:           commit,
		MinTXID:          txID,
		MaxTXID:          txID,
		PreApplyChecksum: preApplyChecksum,
	}); err != nil {
		return fmt.Errorf("cannot encode ltx header: %s", err)
	}

	// Build sorted list of page numbers in current transaction.
	pgnos := make([]uint32, 0, len(txFrameOffsets))
	for pgno := range txFrameOffsets {
		pgnos = append(pgnos, pgno)
	}
	sort.Slice(pgnos, func(i, j int) bool { return pgnos[i] < pgnos[j] })

	frame := make([]byte, walFrameSize)
	var maxOffset int64
	for _, pgno := range pgnos {
		// Read next frame from the WAL file.
		offset := txFrameOffsets[pgno]
		if _, err := walFile.Seek(offset, io.SeekStart); err != nil {
			return fmt.Errorf("seek: %w", err)
		} else if _, err := io.ReadFull(walFile, frame); err != nil {
			return fmt.Errorf("read next frame: %w", err)
		}
		pgno := binary.BigEndian.Uint32(frame[0:4])

		// Copy page into LTX file.
		if err := enc.EncodePage(ltx.PageHeader{Pgno: pgno}, frame[WALFrameHeaderSize:]); err != nil {
			return fmt.Errorf("cannot encode ltx page: pgno=%d err=%w", pgno, err)
		}

		// Track highest offset so we can know where the end of the transaction is in the WAL.
		if offset > maxOffset {
			maxOffset = offset
		}

		// Update rolling checksum.
		postApplyChecksum ^= ltx.ChecksumPage(pgno, frame[WALFrameHeaderSize:])
	}

	// Add truncated pages to page numbers so they can be removed.
	for pgno := commit; pgno < prevPageN; pgno++ {
		pgnos = append(pgnos, pgno)
	}

	// Remove checksums from old pages, if they existed.
	// This can be found either earlier in the WAL or from the database.
	for _, pgno := range pgnos {
		if pgno > prevPageN {
			continue
		}
		if err := db.readPage(dbFile, walFile, pgno, page); err != nil {
			return fmt.Errorf("read page: pgno=%d err=%w", pgno, err)
		}

		postApplyChecksum ^= ltx.ChecksumPage(pgno, page)
	}

	// Ensure checksum flag is applied.
	postApplyChecksum |= ltx.ChecksumFlag

	// Calculate checksum for entire database.
	//if db.store.StrictVerify {
	// TODO: Checksum entire state from database + previous WAL.
	//}

	// Finish page block to compute checksum and then finish header block.
	enc.SetPostApplyChecksum(postApplyChecksum)
	if err := enc.Close(); err != nil {
		return fmt.Errorf("close ltx encoder: %s", err)
	} else if err := f.Sync(); err != nil {
		return fmt.Errorf("sync ltx file: %s", err)
	} else if err := f.Close(); err != nil {
		return fmt.Errorf("close ltx file: %s", err)
	}

	// Atomically rename the file
	if err := os.Rename(tmpPath, ltxPath); err != nil {
		return fmt.Errorf("rename ltx file: %w", err)
	} else if err := internal.Sync(filepath.Dir(ltxPath)); err != nil {
		return fmt.Errorf("sync ltx dir: %w", err)
	}

	// Copy page offsets on commit.
	for pgno, off := range txFrameOffsets {
		db.walFrameOffsets[pgno] = off
	}

	db.pageN = commit
	db.walOffset = maxOffset + walFrameSize

	// Update transaction for database.
	if err := db.setPos(Pos{
		TXID:              enc.Header().MaxTXID,
		PostApplyChecksum: enc.Trailer().PostApplyChecksum,
	}); err != nil {
		return fmt.Errorf("set pos: %w", err)
	}

	// Update metrics
	dbCommitCountMetricVec.WithLabelValues(db.name).Inc()
	dbLTXCountMetricVec.WithLabelValues(db.name).Inc()
	dbLTXBytesMetricVec.WithLabelValues(db.name).Set(float64(enc.N()))

	// Notify store of database change.
	db.store.MarkDirty(db.name)

	return nil

	return nil
}

// readPage reads the latest version of the page before the current transaction.
func (db *DB) readPage(dbFile, walFile *os.File, pgno uint32, buf []byte) error {
	// Read from previous position in WAL, if available.
	if off, ok := db.walFrameOffsets[pgno]; ok {
		if _, err := walFile.Seek(off+WALFrameHeaderSize, io.SeekStart); err != nil {
			return fmt.Errorf("seek wal page: %w", err)
		} else if _, err := io.ReadFull(walFile, buf); err != nil {
			return fmt.Errorf("read wal page: %w", err)
		}
		return nil
	}

	// Otherwise read from the database file.
	if _, err := dbFile.Seek(int64(pgno-1)*int64(db.pageSize), io.SeekStart); err != nil {
		return fmt.Errorf("seek database page: %w", err)
	} else if _, err := io.ReadFull(dbFile, buf); err != nil {
		return fmt.Errorf("read database page: %w", err)
	}
	return nil
}

// CreateSHM creates a new shared memory file on disk.
func (db *DB) CreateSHM() (*os.File, error) {
	return os.OpenFile(db.SHMPath(), os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_TRUNC, 0666)
}

// WriteSHM writes data to the SHM file.
func (db *DB) WriteSHM(f *os.File, data []byte, offset int64) (int, error) {
	dbSHMWriteCountMetricVec.WithLabelValues(db.name).Inc()
	return f.WriteAt(data, offset)
}

// WriteJournal writes data to the rollback journal file.
func (db *DB) WriteJournal(f *os.File, data []byte, offset int64) error {
	if !db.store.IsPrimary() {
		return ErrReadOnlyReplica
	}

	// Assume this is a PERSIST commit if the initial header bytes are cleared.
	if offset == 0 && len(data) == SQLITE_DATABASE_SIZE_OFFSET && isByteSliceZero(data) {
		if err := db.CommitJournal(JournalModePersist); err != nil {
			return fmt.Errorf("commit journal (PERSIST): %w", err)
		}
	}

	_, err := f.WriteAt(data, offset)
	dbJournalWriteCountMetricVec.WithLabelValues(db.name).Inc()
	return err
}

// isByteSliceZero returns true if b only contains NULL bytes.
func isByteSliceZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// CommitJournal deletes the journal file which commits or rolls back the transaction.
func (db *DB) CommitJournal(mode JournalMode) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Return an error if the current process is not the leader.
	if !db.store.IsPrimary() {
		return ErrReadOnlyReplica
	}

	// Read journal header to ensure it's valid.
	if ok, err := db.isJournalHeaderValid(); err != nil {
		return err
	} else if !ok {
		return db.invalidateJournal(mode) // rollback
	}

	// If there is no page size available then nothing has been written.
	// Continue with the invalidation without processing the journal.
	if db.pageSize == 0 {
		if err := db.invalidateJournal(mode); err != nil {
			return fmt.Errorf("invalidate journal: %w", err)
		}
		return nil
	}

	// Determine transaction ID of the in-process transaction.
	pos := db.pos
	txID := pos.TXID + 1

	dbFile, err := os.Open(db.DatabasePath())
	if err != nil {
		return fmt.Errorf("cannot open database file: %w", err)
	}
	defer func() { _ = dbFile.Close() }()

	var commit uint32
	if _, err := dbFile.Seek(SQLITE_DATABASE_SIZE_OFFSET, io.SeekStart); err != nil {
		return fmt.Errorf("cannot seek to database size: %w", err)
	} else if err := binary.Read(dbFile, binary.BigEndian, &commit); err != nil {
		return fmt.Errorf("cannot read database size: %w", err)
	}

	// Compute rolling checksum based off previous LTX database checksum.
	preApplyChecksum := pos.PostApplyChecksum
	postApplyChecksum := pos.PostApplyChecksum // start from previous chksum

	// Remove page checksums from old pages in the journal.
	journalFile, err := os.Open(db.JournalPath())
	if err != nil {
		return fmt.Errorf("cannot open journal file: %w", err)
	}

	journalPageMap, err := buildJournalPageMap(journalFile)
	if err != nil {
		return fmt.Errorf("cannot build journal page map: %w", err)
	}

	for _, pageChksum := range journalPageMap {
		postApplyChecksum ^= pageChksum
	}

	// Build sorted list of dirty page numbers.
	pgnos := make([]uint32, 0, len(db.dirtyPageSet))
	for pgno := range db.dirtyPageSet {
		pgnos = append(pgnos, pgno)
	}
	sort.Slice(pgnos, func(i, j int) bool { return pgnos[i] < pgnos[j] })

	// Open file descriptors for the header & page blocks for new LTX file.
	ltxPath := db.LTXPath(txID, txID)
	tmpPath := ltxPath + ".tmp"
	_ = os.Remove(tmpPath)

	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("cannot create LTX file: %w", err)
	}
	defer func() { _ = f.Close() }()

	enc := ltx.NewEncoder(f)
	if err := enc.EncodeHeader(ltx.Header{
		Version:          1,
		PageSize:         db.pageSize,
		Commit:           commit,
		MinTXID:          txID,
		MaxTXID:          txID,
		PreApplyChecksum: preApplyChecksum,
	}); err != nil {
		return fmt.Errorf("cannot encode ltx header: %s", err)
	}

	// Copy transactions from main database to the LTX file in sorted order.
	buf := make([]byte, db.pageSize)
	dbMode := DBModeRollback
	for _, pgno := range pgnos {
		// Read page from database.
		offset := int64(pgno-1) * int64(db.pageSize)
		if _, err := dbFile.Seek(offset, io.SeekStart); err != nil {
			return fmt.Errorf("cannot seek to database page: pgno=%d err=%w", pgno, err)
		} else if _, err := io.ReadFull(dbFile, buf); err != nil {
			return fmt.Errorf("cannot read database page: pgno=%d err=%w", pgno, err)
		}

		// Copy page into LTX file.
		if err := enc.EncodePage(ltx.PageHeader{Pgno: pgno}, buf); err != nil {
			return fmt.Errorf("cannot encode ltx page: pgno=%d err=%w", pgno, err)
		}

		// Update the mode if this is the first page and the write/read versions as set to WAL (2).
		if pgno == 1 && buf[18] == 2 && buf[19] == 2 {
			dbMode = DBModeWAL
		}

		// Update rolling checksum.
		postApplyChecksum ^= ltx.ChecksumPage(pgno, buf)
	}

	// Checksum pages removed by truncation.
	if _, err := dbFile.Seek(int64(commit)*int64(db.pageSize), io.SeekStart); err != nil {
		return fmt.Errorf("cannot seek to end of database: %w", err)
	}
	for pgno := commit + 1; ; pgno++ {
		if _, err := io.ReadFull(dbFile, buf); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("cannot read truncated database page: pgno=%d err=%w", pgno, err)
		}
		postApplyChecksum ^= ltx.ChecksumPage(pgno, buf)
	}

	// Ensure checksum flag is applied.
	postApplyChecksum |= ltx.ChecksumFlag

	// Calculate checksum for entire database.
	if db.store.StrictVerify {
		if _, err := dbFile.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seek database file: %w", err)
		}

		lr := &io.LimitedReader{R: dbFile, N: int64(commit) * int64(db.pageSize)}

		chksum, err := ltx.ChecksumReader(lr, int(db.pageSize))
		if err != nil {
			return fmt.Errorf("checksum database: %w", err)
		} else if chksum != postApplyChecksum {
			return fmt.Errorf("ltx post-apply checksum mismatch: %016x <> %016x", chksum, postApplyChecksum)
		}
	}

	// Finish page block to compute checksum and then finish header block.
	enc.SetPostApplyChecksum(postApplyChecksum)
	if err := enc.Close(); err != nil {
		return fmt.Errorf("close ltx encoder: %s", err)
	} else if err := f.Sync(); err != nil {
		return fmt.Errorf("sync ltx file: %s", err)
	} else if err := f.Close(); err != nil {
		return fmt.Errorf("close ltx file: %s", err)
	}

	// Atomically rename the file
	if err := os.Rename(tmpPath, ltxPath); err != nil {
		return fmt.Errorf("rename ltx file: %w", err)
	} else if err := internal.Sync(filepath.Dir(ltxPath)); err != nil {
		return fmt.Errorf("sync ltx dir: %w", err)
	}

	// Ensure file is persisted to disk.
	if err := dbFile.Sync(); err != nil {
		return fmt.Errorf("cannot sync ltx file: %w", err)
	}

	if err := db.invalidateJournal(mode); err != nil {
		return fmt.Errorf("invalidate journal: %w", err)
	}

	// Update database flags.
	db.pageN = commit
	db.mode = dbMode

	// Update transaction for database.
	if err := db.setPos(Pos{
		TXID:              enc.Header().MaxTXID,
		PostApplyChecksum: enc.Trailer().PostApplyChecksum,
	}); err != nil {
		return fmt.Errorf("set pos: %w", err)
	}

	// Update metrics
	dbCommitCountMetricVec.WithLabelValues(db.name).Inc()
	dbLTXCountMetricVec.WithLabelValues(db.name).Inc()
	dbLTXBytesMetricVec.WithLabelValues(db.name).Set(float64(enc.N()))

	// Notify store of database change.
	db.store.MarkDirty(db.name)

	return nil
}

// isJournalHeaderValid returns true if the journal starts with the journal magic.
func (db *DB) isJournalHeaderValid() (bool, error) {
	f, err := os.Open(db.JournalPath())
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, len(SQLITE_JOURNAL_HEADER_STRING))
	if _, err := io.ReadFull(f, buf); err != nil {
		return false, err
	}
	return string(buf) == SQLITE_JOURNAL_HEADER_STRING, nil
}

// invalidateJournal invalidates the journal file based on the journal mode.
func (db *DB) invalidateJournal(mode JournalMode) error {
	switch mode {
	case JournalModeDelete:
		if err := os.Remove(db.JournalPath()); err != nil {
			return fmt.Errorf("remove journal file: %w", err)
		}

	case JournalModeTruncate:
		if err := os.Truncate(db.JournalPath(), 0); err != nil {
			return fmt.Errorf("truncate: %w", err)
		} else if err := internal.Sync(db.JournalPath()); err != nil {
			return fmt.Errorf("sync journal: %w", err)
		}

	case JournalModePersist:
		f, err := os.OpenFile(db.JournalPath(), os.O_RDWR, 0666)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("open journal: %w", err)
		}
		defer func() { _ = f.Close() }()

		if _, err := f.Write(make([]byte, SQLITE_JOURNAL_HEADER_SIZE)); err != nil {
			return fmt.Errorf("clear journal header: %w", err)
		} else if err := f.Sync(); err != nil {
			return fmt.Errorf("sync journal: %w", err)
		} else if err := f.Close(); err != nil {
			return fmt.Errorf("close journal: %w", err)
		}

	default:
		return fmt.Errorf("invalid journal: %q", mode)
	}

	// Sync the underlying directory.
	if err := internal.Sync(db.path); err != nil {
		return fmt.Errorf("sync database directory: %w", err)
	}

	db.dirtyPageSet = make(map[uint32]struct{})

	return nil
}

// ApplyLTX applies an LTX file to the database.
func (db *DB) ApplyLTX(ctx context.Context, path string) error {
	guard, err := db.AcquireWriteLock(ctx)
	if err != nil {
		return err
	}
	defer guard.Unlock()

	// Open database file for writing.
	dbf, err := os.OpenFile(db.DatabasePath(), os.O_RDWR, 0666)
	if err != nil {
		return fmt.Errorf("open database file: %w", err)
	}
	defer func() { _ = dbf.Close() }()

	// Open LTX header reader.
	hf, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer func() { _ = hf.Close() }()

	dec := ltx.NewDecoder(hf)
	if err := dec.DecodeHeader(); err != nil {
		return fmt.Errorf("decode ltx header: %s", err)
	}

	dbMode := db.mode
	pageBuf := make([]byte, dec.Header().PageSize)
	for i := 0; ; i++ {
		// Read pgno & page data from LTX file.
		var phdr ltx.PageHeader
		if err := dec.DecodePage(&phdr, pageBuf); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("decode ltx page[%d]: %w", i, err)
		}

		// Update the mode if this is the first page and the write/read versions as set to WAL (2).
		if phdr.Pgno == 1 && pageBuf[18] == 2 && pageBuf[19] == 2 {
			dbMode = DBModeWAL
		}

		// Copy to database file.
		offset := int64(phdr.Pgno-1) * int64(dec.Header().PageSize)
		if _, err := dbf.WriteAt(pageBuf, offset); err != nil {
			return fmt.Errorf("write to database file: %w", err)
		}

		// Invalidate page cache.
		if invalidator := db.store.Invalidator; invalidator != nil {
			if err := invalidator.InvalidateDB(db, offset, int64(len(pageBuf))); err != nil {
				return fmt.Errorf("invalidate db: %w", err)
			}
		}
	}

	// Close the reader so we can verify file integrity.
	if err := dec.Close(); err != nil {
		return fmt.Errorf("close ltx decode: %w", err)
	}

	// Truncate database file to size after LTX file.
	if err := dbf.Truncate(int64(dec.Header().Commit) * int64(dec.Header().PageSize)); err != nil {
		return fmt.Errorf("truncate database file: %w", err)
	}

	// Sync changes to disk.
	if err := dbf.Sync(); err != nil {
		return fmt.Errorf("sync database file: %w", err)
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	db.mode = dbMode

	if db.pageSize == 0 {
		db.pageSize = dec.Header().PageSize
	}

	// Update transaction for database.
	if err := db.setPos(Pos{
		TXID:              dec.Header().MaxTXID,
		PostApplyChecksum: dec.Trailer().PostApplyChecksum,
	}); err != nil {
		return fmt.Errorf("set pos: %w", err)
	}

	// Invalidate SHM so that the transaction is visible.
	if err := db.invalidateSHM(ctx); err != nil {
		return fmt.Errorf("invalidate shm: %w", err)
	}

	// Notify store of database change.
	db.store.MarkDirty(db.name)

	return nil
}

// invalidateSHM clears the SHM header so that SQLite needs to rebuild it.
func (db *DB) invalidateSHM(ctx context.Context) error {
	f, err := os.OpenFile(db.SHMPath(), os.O_RDWR, 0666)
	if os.IsNotExist(err) {
		return nil // no shm, exit
	} else if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// Invalidate page cache.
	if invalidator := db.store.Invalidator; invalidator != nil {
		if err := invalidator.InvalidateSHM(db); err != nil {
			return err
		}
	}

	// Clear the SHM file header.
	if _, err := f.Write(make([]byte, 135)); err != nil {
		return err
	}

	return nil
}

// AcquireWriteLock acquires the appropriate locks for a write depending on if
// the database uses a rollback journal or WAL.
func (db *DB) AcquireWriteLock(ctx context.Context) (_ *GuardSet, err error) {
	gs := db.GuardSet()
	defer func() {
		if err != nil {
			gs.Unlock()
		}
	}()

	// Acquire shared lock to check database mode.
	if err := gs.pending.RLock(ctx); err != nil {
		return nil, fmt.Errorf("acquire PENDING read lock: %w", err)
	}
	if err := gs.shared.RLock(ctx); err != nil {
		return nil, fmt.Errorf("acquire SHARED read lock: %w", err)
	}
	gs.pending.Unlock()

	// If this is a rollback journal, upgrade all database locks to exclusive.
	if db.mode == DBModeRollback {
		if err := gs.reserved.Lock(ctx); err != nil {
			return nil, fmt.Errorf("acquire RESERVED write lock: %w", err)
		}
		if err := gs.pending.Lock(ctx); err != nil {
			return nil, fmt.Errorf("acquire PENDING write lock: %w", err)
		}
		if err := gs.shared.Lock(ctx); err != nil {
			return nil, fmt.Errorf("acquire SHARED write lock: %w", err)
		}
		return gs, nil
	}

	if err := gs.write.Lock(ctx); err != nil {
		return nil, fmt.Errorf("acquire exclusive WAL_WRITE_LOCK: %w", err)
	}
	if err := gs.ckpt.Lock(ctx); err != nil {
		return nil, fmt.Errorf("acquire exclusive WAL_CKPT_LOCK: %w", err)
	}
	if err := gs.recover.Lock(ctx); err != nil {
		return nil, fmt.Errorf("acquire exclusive WAL_RECOVER_LOCK: %w", err)
	}
	if err := gs.read0.Lock(ctx); err != nil {
		return nil, fmt.Errorf("acquire exclusive WAL_READ0_LOCK: %w", err)
	}
	if err := gs.read1.Lock(ctx); err != nil {
		return nil, fmt.Errorf("acquire exclusive WAL_READ1_LOCK: %w", err)
	}
	if err := gs.read2.Lock(ctx); err != nil {
		return nil, fmt.Errorf("acquire exclusive WAL_READ2_LOCK: %w", err)
	}
	if err := gs.read3.Lock(ctx); err != nil {
		return nil, fmt.Errorf("acquire exclusive WAL_READ3_LOCK: %w", err)
	}
	if err := gs.read4.Lock(ctx); err != nil {
		return nil, fmt.Errorf("acquire exclusive WAL_READ4_LOCK: %w", err)
	}

	return gs, nil
}

// GuardSet returns a set of guards that can control locking for the database file.
func (db *DB) GuardSet() *GuardSet {
	return &GuardSet{
		pending:  db.pendingLock.Guard(),
		shared:   db.sharedLock.Guard(),
		reserved: db.reservedLock.Guard(),

		write:   db.writeLock.Guard(),
		ckpt:    db.ckptLock.Guard(),
		recover: db.recoverLock.Guard(),
		read0:   db.read0Lock.Guard(),
		read1:   db.read1Lock.Guard(),
		read2:   db.read2Lock.Guard(),
		read3:   db.read3Lock.Guard(),
		read4:   db.read4Lock.Guard(),
		dms:     db.dmsLock.Guard(),
	}
}

// InWriteTx returns true if the RESERVED lock has an exclusive lock.
func (db *DB) InWriteTx() bool {
	return db.reservedLock.State() == RWMutexStateExclusive
}

// WriteSnapshotTo writes an LTX snapshot to dst.
func (db *DB) WriteSnapshotTo(ctx context.Context, dst io.Writer) (header ltx.Header, trailer ltx.Trailer, err error) {
	gs := db.GuardSet()
	defer gs.Unlock()

	// Acquire PENDING then SHARED. Release PENDING immediately afterward.
	if err := gs.pending.RLock(ctx); err != nil {
		return header, trailer, fmt.Errorf("acquire PENDING read lock: %w", err)
	}
	if err := gs.shared.RLock(ctx); err != nil {
		return header, trailer, fmt.Errorf("acquire SHARED read lock: %w", err)
	}
	gs.pending.Unlock()

	// Acquire the READ0 lock to prevent checkpointing, in case this is in WAL mode.
	if err := gs.read0.RLock(ctx); err != nil {
		return header, trailer, fmt.Errorf("acquire READ0 read lock: %w", err)
	}

	// Determine current position & snapshot overriding WAL frames.
	db.mu.Lock()
	pos := db.pos
	pageSize, pageN := db.pageSize, db.pageN
	walFrameOffsets := make(map[uint32]int64, len(db.walFrameOffsets))
	for k, v := range db.walFrameOffsets {
		walFrameOffsets[k] = v
	}
	db.mu.Unlock()

	// Open database file.
	dbFile, err := os.Open(db.DatabasePath())
	if err != nil {
		return header, trailer, fmt.Errorf("open database file: %w", err)
	}
	defer func() { _ = dbFile.Close() }()

	// Open WAL file if we have overriding WAL frames.
	var walFile *os.File
	if len(walFrameOffsets) > 0 {
		if walFile, err = os.Open(db.WALPath()); err != nil {
			return header, trailer, fmt.Errorf("open wal file: %w", err)
		}
		defer func() { _ = walFile.Close() }()
	}

	// Write current database state to an LTX writer.
	enc := ltx.NewEncoder(dst)
	if err := enc.EncodeHeader(ltx.Header{
		Version:   ltx.Version,
		PageSize:  pageSize,
		Commit:    pageN,
		MinTXID:   1,
		MaxTXID:   pos.TXID,
		Timestamp: uint64(db.Now().UnixMilli()),
	}); err != nil {
		return header, trailer, fmt.Errorf("encode ltx header: %w", err)
	}

	// Write page frames.
	pageData := make([]byte, pageSize)
	var chksum uint64
	for pgno := uint32(1); pgno <= pageN; pgno++ {
		// Read from WAL if page exists in offset map. Otherwise read from DB.
		if walFrameOffset, ok := walFrameOffsets[pgno]; ok {
			if _, err := walFile.Seek(walFrameOffset+WALFrameHeaderSize, io.SeekStart); err != nil {
				return header, trailer, fmt.Errorf("seek wal page: %w", err)
			} else if _, err := io.ReadFull(walFile, pageData); err != nil {
				return header, trailer, fmt.Errorf("read wal page: %w", err)
			}
		} else {
			if _, err := dbFile.Seek(int64(pgno-1)*int64(pageSize), io.SeekStart); err != nil {
				return header, trailer, fmt.Errorf("seek database page: %w", err)
			} else if _, err := io.ReadFull(dbFile, pageData); err != nil {
				return header, trailer, fmt.Errorf("read database page: %w", err)
			}
		}

		if err := enc.EncodePage(ltx.PageHeader{Pgno: pgno}, pageData); err != nil {
			return header, trailer, fmt.Errorf("encode page frame: %w", err)
		}

		chksum ^= ltx.ChecksumPage(pgno, pageData)
	}

	// Set the database checksum before we write the trailer.
	postApplyChecksum := ltx.ChecksumFlag | chksum
	if postApplyChecksum != pos.PostApplyChecksum {
		return header, trailer, fmt.Errorf("snapshot checksum mismatch at tx %s: %x <> %x", ltx.FormatTXID(pos.TXID), postApplyChecksum, pos.PostApplyChecksum)
	}
	enc.SetPostApplyChecksum(postApplyChecksum)

	if err := enc.Close(); err != nil {
		return header, trailer, fmt.Errorf("close ltx encoder: %w", err)
	}

	return enc.Header(), enc.Trailer(), nil
}

// EnforceRetention removes all LTX files created before minTime.
func (db *DB) EnforceRetention(ctx context.Context, minTime time.Time) error {
	// Collect all LTX files.
	ents, err := db.ReadLTXDir()
	if err != nil {
		return fmt.Errorf("read ltx dir: %w", err)
	} else if len(ents) == 0 {
		return nil // no LTX files, exit
	}

	// Ensure the latest LTX file is not removed.
	ents = ents[:len(ents)-1]

	// Delete all files that are before the minimum time.
	var totalN int
	var totalSize int64
	for _, ent := range ents {
		// Check if file qualifies for deletion.
		fi, err := ent.Info()
		if err != nil {
			return fmt.Errorf("info: %w", err)
		} else if fi.ModTime().After(minTime) {
			totalN++
			totalSize += fi.Size()
			continue // after minimum time, skip
		}

		// Remove file if it passes all the checks.
		filename := filepath.Join(db.LTXDir(), ent.Name())
		if err := os.Remove(filename); err != nil {
			return err
		}

		// Update metrics.
		dbLTXReapCountMetricVec.WithLabelValues(db.name).Inc()
	}

	// Reset metrics for LTX disk usage.
	dbLTXCountMetricVec.WithLabelValues(db.name).Set(float64(totalN))
	dbLTXBytesMetricVec.WithLabelValues(db.name).Set(float64(totalSize))

	return nil
}

type dbVarJSON struct {
	Name     string `json:"name"`
	PageSize uint32 `json:"pageSize"`
	TXID     string `json:"txid"`
	Checksum string `json:"checksum"`

	Locks struct {
		Pending  string `json:"pending"`
		Shared   string `json:"shared"`
		Reserved string `json:"reserved"`

		Write   string `json:"write"`
		Ckpt    string `json:"ckpt"`
		Recover string `json:"recover"`
		Read0   string `json:"read0"`
		Read1   string `json:"read1"`
		Read2   string `json:"read2"`
		Read3   string `json:"read3"`
		Read4   string `json:"read4"`
		DMS     string `json:"dms"`
	} `json:"locks"`
}

func buildJournalPageMap(f *os.File) (map[uint32]uint64, error) {
	r := NewJournalReader(f)

	// Generate a map of pages and their new checksums.
	m := make(map[uint32]uint64)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	for i := 0; ; i++ {
		if err := r.Next(); err == io.EOF {
			return m, nil
		} else if err != nil {
			return m, fmt.Errorf("journal segment(%d): %w", i, err)
		}

		for j := 0; ; j++ {
			pgno, data, err := r.ReadFrame()
			if err == io.EOF {
				break
			} else if err != nil {
				return m, fmt.Errorf("journal segment(%d) frame(%d): %w", i, j, err)
			}

			// Calculate LTX page checksum and add it to the map.
			chksum := ltx.ChecksumPage(pgno, data)
			m[pgno] = chksum
		}
	}
}

// JouralReader represents a reader of the SQLite journal file format.
type JournalReader struct {
	r     io.Reader
	n     int64  // bytes read
	frame []byte // frame buffer

	frameN      int32  // Number of pages in the segment, or -1 to mean all content to the end of the file
	nonce       uint32 // A random nonce for the checksum
	initialSize uint32 // Initial size of the database in pages
	sectorSize  uint32 // Size of the disk sectors
	pageSize    uint32 // Size of each page, in bytes
}

// JournalReader returns a new instance of JournalReader.
func NewJournalReader(r io.Reader) *JournalReader {
	return &JournalReader{r: r}
}

// DatabaseSize returns the size of the database before the journal transaction, in bytes.
func (r *JournalReader) DatabaseSize() int64 {
	return int64(r.initialSize) * int64(r.pageSize)
}

// Next reads the next segment of the journal. Returns io.EOF if no more segments exist.
func (r *JournalReader) Next() error {
	// Read journal header. Return EOF if header is invalid.
	buf := make([]byte, len(SQLITE_JOURNAL_HEADER_STRING)+20)
	n, err := io.ReadFull(r.r, buf)
	r.n += int64(n)
	if err != nil {
		return io.EOF
	} else if string(buf[:len(SQLITE_JOURNAL_HEADER_STRING)]) != SQLITE_JOURNAL_HEADER_STRING {
		return io.EOF
	}

	// Read fields after header magic.
	hdr := buf[len(SQLITE_JOURNAL_HEADER_STRING):]
	r.frameN = int32(binary.BigEndian.Uint32(hdr[0:]))
	r.nonce = binary.BigEndian.Uint32(hdr[4:])
	r.initialSize = binary.BigEndian.Uint32(hdr[8:])
	r.sectorSize = binary.BigEndian.Uint32(hdr[12:])
	r.pageSize = binary.BigEndian.Uint32(hdr[16:])
	if r.pageSize == 0 {
		return fmt.Errorf("invalid page size in journal header: %d", r.pageSize)
	}

	// Create a buffer to read the segment frames.
	r.frame = make([]byte, r.pageSize+4+4)

	// Move to the end of the sector.
	p := make([]byte, int64(r.sectorSize)-int64(len(buf)))
	n, err = io.ReadFull(r.r, p)
	r.n += int64(n)
	if err != nil && err != io.EOF {
		return fmt.Errorf("cannot seek to next sector: %w", err)
	}

	return nil
}

// ReadFrame returns the page number and page data for the next frame.
// Returns io.EOF after the last frame. Page data should not be retained.
func (r *JournalReader) ReadFrame() (pgno uint32, data []byte, err error) {
	// No more frames, exit.
	if r.frameN == 0 {
		return 0, nil, io.EOF
	}

	// Read the next frame from the journal.
	n, err := io.ReadFull(r.r, r.frame)
	r.n += int64(n)
	if err != nil {
		return 0, nil, err
	}
	r.frameN--

	// At the end of the last frame, move to the next sector.
	if r.frameN == 0 {
		b := make([]byte, nextMultipleOf(r.n, int64(r.sectorSize))-r.n)
		n, err := io.ReadFull(r.r, b)
		r.n += int64(n)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return 0, nil, fmt.Errorf("seek to next journal segment: %w", err)
		}
	}

	return binary.BigEndian.Uint32(r.frame[0:]), r.frame[4 : len(r.frame)-4], nil
}

// nextMultipleOf returns the next multiple of denom based on v.
// Returns v if it is a multiple of denom.
func nextMultipleOf(v, denom int64) int64 {
	mod := v % denom
	if mod == 0 {
		return v
	}
	return v + (denom - mod)
}

// readAndVerifyLTXFile reads an LTX file and verifies its integrity.
// Returns the header & the trailer from the file.
func readAndVerifyLTXFile(filename string) (ltx.Header, ltx.Trailer, error) {
	f, err := os.Open(filename)
	if err != nil {
		return ltx.Header{}, ltx.Trailer{}, err
	}
	defer func() { _ = f.Close() }()

	r := ltx.NewReader(f)
	if _, err := io.Copy(io.Discard, r); err != nil {
		return ltx.Header{}, ltx.Trailer{}, err
	}
	return r.Header(), r.Trailer(), nil
}

// TrimName removes "-journal", "-shm" or "-wal" from the given name.
func TrimName(name string) string {
	if suffix := "-journal"; strings.HasSuffix(name, suffix) {
		name = strings.TrimSuffix(name, suffix)
	}
	if suffix := "-wal"; strings.HasSuffix(name, suffix) {
		name = strings.TrimSuffix(name, suffix)
	}
	if suffix := "-shm"; strings.HasSuffix(name, suffix) {
		name = strings.TrimSuffix(name, suffix)
	}
	return name
}

const (
	SQLITE_DATABASE_HEADER_STRING = "SQLite format 3\x00"

	// Location of the database size, in pages, in the main database file.
	SQLITE_DATABASE_SIZE_OFFSET = 28

	/// Magic header string that identifies a SQLite journal header.
	/// https://www.sqlite.org/fileformat.html#the_rollback_journal
	SQLITE_JOURNAL_HEADER_STRING = "\xd9\xd5\x05\xf9\x20\xa1\x63\xd7"

	// Size of the journal header, in bytes.
	SQLITE_JOURNAL_HEADER_SIZE = 28
)

// LockType represents a SQLite lock type.
type LockType int

const (
	// Database file locks
	LockTypePending  = LockType(0x40000000) // 1073741824
	LockTypeReserved = LockType(0x40000001) // 1073741825
	LockTypeShared   = LockType(0x40000002) // 1073741826

	// SHM file locks
	LockTypeWrite   = LockType(120)
	LockTypeCkpt    = LockType(121)
	LockTypeRecover = LockType(122)
	LockTypeRead0   = LockType(123)
	LockTypeRead1   = LockType(124)
	LockTypeRead2   = LockType(125)
	LockTypeRead3   = LockType(126)
	LockTypeRead4   = LockType(127)
	LockTypeDMS     = LockType(128)
)

// ParseDatabaseLockRange returns a list of SQLite database locks that are within a range.
func ParseDatabaseLockRange(start, end uint64) []LockType {
	a := make([]LockType, 0, 3)
	if start <= uint64(LockTypePending) && uint64(LockTypePending) <= end {
		a = append(a, LockTypePending)
	}
	if start <= uint64(LockTypeReserved) && uint64(LockTypeReserved) <= end {
		a = append(a, LockTypeReserved)
	}
	if start <= uint64(LockTypeShared) && uint64(LockTypeShared) <= end {
		a = append(a, LockTypeShared)
	}
	return a
}

// ParseWALLockRange returns a list of SQLite WAL locks that are within a range.
func ParseWALLockRange(start, end uint64) []LockType {
	a := make([]LockType, 0, 3)
	if start <= uint64(LockTypeWrite) && uint64(LockTypeWrite) <= end {
		a = append(a, LockTypeWrite)
	}
	if start <= uint64(LockTypeCkpt) && uint64(LockTypeCkpt) <= end {
		a = append(a, LockTypeCkpt)
	}
	if start <= uint64(LockTypeRecover) && uint64(LockTypeRecover) <= end {
		a = append(a, LockTypeRecover)
	}

	if start <= uint64(LockTypeRead0) && uint64(LockTypeRead0) <= end {
		a = append(a, LockTypeRead0)
	}
	if start <= uint64(LockTypeRead1) && uint64(LockTypeRead1) <= end {
		a = append(a, LockTypeRead1)
	}
	if start <= uint64(LockTypeRead2) && uint64(LockTypeRead2) <= end {
		a = append(a, LockTypeRead2)
	}
	if start <= uint64(LockTypeRead3) && uint64(LockTypeRead3) <= end {
		a = append(a, LockTypeRead3)
	}
	if start <= uint64(LockTypeRead4) && uint64(LockTypeRead4) <= end {
		a = append(a, LockTypeRead4)
	}
	if start <= uint64(LockTypeDMS) && uint64(LockTypeDMS) <= end {
		a = append(a, LockTypeDMS)
	}

	return a
}

// GuardSet represents a set of mutex guards by a single owner.
type GuardSet struct {
	// Database file locks
	pending  RWMutexGuard
	shared   RWMutexGuard
	reserved RWMutexGuard

	// SHM file locks
	write   RWMutexGuard
	ckpt    RWMutexGuard
	recover RWMutexGuard
	read0   RWMutexGuard
	read1   RWMutexGuard
	read2   RWMutexGuard
	read3   RWMutexGuard
	read4   RWMutexGuard
	dms     RWMutexGuard
}

// Guard returns a guard by lock type. Panic on invalid lock type.
func (s *GuardSet) Guard(lockType LockType) *RWMutexGuard {
	switch lockType {

	// Database locks
	case LockTypePending:
		return &s.pending
	case LockTypeShared:
		return &s.shared
	case LockTypeReserved:
		return &s.reserved

	// SHM file locks
	case LockTypeWrite:
		return &s.write
	case LockTypeCkpt:
		return &s.ckpt
	case LockTypeRecover:
		return &s.recover
	case LockTypeRead0:
		return &s.read0
	case LockTypeRead1:
		return &s.read1
	case LockTypeRead2:
		return &s.read2
	case LockTypeRead3:
		return &s.read3
	case LockTypeRead4:
		return &s.read4
	case LockTypeDMS:
		return &s.dms

	default:
		panic("GuardSet.Guard(): invalid database lock type")
	}
}

// Unlock unlocks all the guards in reversed order that they are acquired by SQLite.
func (s *GuardSet) Unlock() {
	s.UnlockDatabase()
	s.UnlockSHM()
}

// UnlockDatabase unlocks all the database file guards.
func (s *GuardSet) UnlockDatabase() {
	s.pending.Unlock()
	s.shared.Unlock()
	s.reserved.Unlock()
}

// UnlockSHM unlocks all the SHM file guards.
func (s *GuardSet) UnlockSHM() {
	s.write.Unlock()
	s.ckpt.Unlock()
	s.recover.Unlock()
	s.read0.Unlock()
	s.read1.Unlock()
	s.read2.Unlock()
	s.read3.Unlock()
	s.read4.Unlock()
	s.dms.Unlock()
}

// SQLite constants
const (
	databaseHeaderSize = 100
)

type sqliteDatabaseHeader struct {
	WriteVersion int
	ReadVersion  int
	PageSize     uint32
	PageN        uint32
}

// readSQLiteDatabaseHeader reads specific fields from the header of a SQLite database file.
func readSQLiteDatabaseHeader(r io.Reader) (hdr sqliteDatabaseHeader, err error) {
	b := make([]byte, databaseHeaderSize)
	if _, err := io.ReadFull(r, b); err != nil {
		return hdr, err
	} else if !bytes.Equal(b[:len(SQLITE_DATABASE_HEADER_STRING)], []byte(SQLITE_DATABASE_HEADER_STRING)) {
		return hdr, fmt.Errorf("invalid sqlite database file: %x <> %x", b[:len(SQLITE_DATABASE_HEADER_STRING)], SQLITE_DATABASE_HEADER_STRING)
	}

	hdr.WriteVersion = int(b[18])
	hdr.ReadVersion = int(b[19])

	hdr.PageSize = uint32(binary.BigEndian.Uint16(b[16:]))
	if hdr.PageSize == 1 {
		hdr.PageSize = 65536
	}
	if !ltx.IsValidPageSize(hdr.PageSize) {
		return hdr, fmt.Errorf("invalid sqlite page size: %d", hdr.PageSize)
	}

	hdr.PageN = binary.BigEndian.Uint32(b[28:])

	return hdr, nil
}

// Database metrics.
var (
	dbTXIDMetricVec = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "litefs_db_txid",
		Help: "Current transaction ID.",
	}, []string{"db"})

	dbDatabaseWriteCountMetricVec = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "litefs_db_database_write_count",
		Help: "Number of writes to the database file.",
	}, []string{"db"})

	dbJournalWriteCountMetricVec = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "litefs_db_journal_write_count",
		Help: "Number of writes to the journal file.",
	}, []string{"db"})

	dbWALWriteCountMetricVec = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "litefs_db_wal_write_count",
		Help: "Number of writes to the WAL file.",
	}, []string{"db"})

	dbSHMWriteCountMetricVec = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "litefs_db_shm_write_count",
		Help: "Number of writes to the shared memory file.",
	}, []string{"db"})

	dbCommitCountMetricVec = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "litefs_db_commit_count",
		Help: "Number of database commits.",
	}, []string{"db"})

	dbLTXCountMetricVec = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "litefs_db_ltx_count",
		Help: "Number of LTX files on disk.",
	}, []string{"db"})

	dbLTXBytesMetricVec = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "litefs_db_ltx_bytes",
		Help: "Number of bytes used by LTX files on disk.",
	}, []string{"db"})

	dbLTXReapCountMetricVec = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "litefs_db_ltx_reap_count",
		Help: "Number of LTX files removed by retention.",
	}, []string{"db"})
)
