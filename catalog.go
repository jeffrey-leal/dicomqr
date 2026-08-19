package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

// catalogFileName is the SQLite index stored inside the download directory.
// The folder scanner only considers *.dcm files, so the index and its WAL
// side files are never picked up as DICOM data.
const catalogFileName = ".dicomqr-index.db"

// catalogSchema mirrors the Patient → Study → Series → Instance hierarchy of
// the Local Browse tree. Instances are keyed by absolute file path; empty
// parents are pruned bottom-up in removePaths, so the REFERENCES clauses are
// documentation rather than enforced constraints.
const catalogSchema = `
CREATE TABLE IF NOT EXISTS patients (
	patient_id   TEXT PRIMARY KEY,
	patient_name TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS studies (
	study_uid  TEXT PRIMARY KEY,
	patient_id TEXT NOT NULL REFERENCES patients(patient_id),
	study_date TEXT NOT NULL DEFAULT '',
	study_desc TEXT NOT NULL DEFAULT '',
	accession  TEXT NOT NULL DEFAULT '',
	modalities TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS series (
	series_uid    TEXT PRIMARY KEY,
	study_uid     TEXT NOT NULL REFERENCES studies(study_uid),
	modality      TEXT NOT NULL DEFAULT '',
	series_number TEXT NOT NULL DEFAULT '',
	series_desc   TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS instances (
	path       TEXT PRIMARY KEY,
	series_uid TEXT NOT NULL REFERENCES series(series_uid),
	size       INTEGER NOT NULL DEFAULT 0,
	mtime      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_studies_patient  ON studies(patient_id);
CREATE INDEX IF NOT EXISTS idx_series_study     ON series(study_uid);
CREATE INDEX IF NOT EXISTS idx_instances_series ON instances(series_uid);
`

// catalog is the persistent index of the download directory backing the Local
// Browse tree. All methods are safe on a nil receiver (the feature degrades to
// the previous in-memory behaviour) and safe for concurrent use from the UI,
// scan, retrieve, and SCP goroutines: mu serialises every operation, and the
// single sql connection (SetMaxOpenConns(1)) avoids SQLITE_BUSY.
type catalog struct {
	mu  sync.Mutex
	dir string
	db  *sql.DB
}

// openCatalog opens (creating if necessary) the index for dir.
func openCatalog(dir string) (*catalog, error) {
	c := &catalog{}
	if err := c.Reopen(dir); err != nil {
		return nil, err
	}
	return c, nil
}

// Reopen closes any open index and opens the one inside dir. Called when the
// download directory changes in Preferences; each directory carries its own
// index file. An empty dir leaves the catalog open-but-inert.
func (c *catalog) Reopen(dir string) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db != nil {
		c.db.Close()
		c.db = nil
	}
	c.dir = dir
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, catalogFileName))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return err
		}
	}
	if _, err := db.Exec(catalogSchema); err != nil {
		db.Close()
		return err
	}
	c.db = db
	return nil
}

func (c *catalog) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db != nil {
		c.db.Close()
		c.db = nil
	}
}

// Dir returns the directory whose index is currently open.
func (c *catalog) Dir() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dir
}

// replaceAll wipes the index and repopulates it from a full folder scan.
func (c *catalog) replaceAll(studies []localStudy, series []localSeries, filesByUID map[string][]string) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil {
		return nil
	}
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, stmt := range []string{
		"DELETE FROM instances", "DELETE FROM series",
		"DELETE FROM studies", "DELETE FROM patients",
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}

	for _, s := range studies {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO patients(patient_id, patient_name) VALUES(?, ?)`,
			s.patientID, s.patientName); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO studies(study_uid, patient_id, study_date, study_desc, accession, modalities)
			VALUES(?, ?, ?, ?, ?, ?)`,
			s.studyUID, s.patientID, s.studyDate, s.studyDesc, s.accession, s.modalities); err != nil {
			return err
		}
	}
	for _, sr := range series {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO series(series_uid, study_uid, modality, series_number, series_desc)
			VALUES(?, ?, ?, ?, ?)`,
			sr.seriesUID, sr.studyUID, sr.modality, sr.seriesNumber, sr.seriesDesc); err != nil {
			return err
		}
	}
	for seriesUID, paths := range filesByUID {
		for _, p := range paths {
			var size, mtime int64
			if info, statErr := os.Stat(p); statErr == nil {
				size, mtime = info.Size(), info.ModTime().Unix()
			}
			if _, err := tx.Exec(`INSERT OR REPLACE INTO instances(path, series_uid, size, mtime) VALUES(?, ?, ?, ?)`,
				p, seriesUID, size, mtime); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// upsertMeta inserts one parsed file into the hierarchy. Patient, study, and
// series rows are first-write-wins (INSERT OR IGNORE), matching how the tree
// model keeps the metadata of the first file seen.
func upsertMeta(tx *sql.Tx, m fileMeta) error {
	if _, err := tx.Exec(`INSERT OR IGNORE INTO patients(patient_id, patient_name) VALUES(?, ?)`,
		m.patientID, m.patientName); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO studies(study_uid, patient_id, study_date, study_desc, accession, modalities)
		VALUES(?, ?, ?, ?, ?, ?)`,
		m.studyUID, m.patientID, m.studyDate, m.studyDesc, m.accession, m.modalities); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO series(series_uid, study_uid, modality, series_number, series_desc)
		VALUES(?, ?, ?, ?, ?)`,
		m.seriesUID, m.studyUID, m.modality, m.seriesNumber, m.seriesDesc); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT OR REPLACE INTO instances(path, series_uid, size, mtime) VALUES(?, ?, ?, ?)`,
		m.path, m.seriesUID, m.size, m.mtime)
	return err
}

// fileStamp is what the index remembers about a file's contents without
// reading them: enough to tell whether the copy on disk is still the one that
// was parsed.
type fileStamp struct{ size, mtime int64 }

// fileStamps returns every indexed file's path and stamp.
//
// These two columns have been written on every scan and ingest since the index
// was added and never read back. They are what lets a re-scan skip a file: an
// entry whose size and modification time still match the index cannot have
// changed since it was parsed, so there is nothing to learn by opening it.
//
// Resolution is one second, since that is what the index stores. A file
// rewritten within the same second to exactly the same length would look
// unchanged — which dicomqr never does to its own downloads, and which is what
// the Rebuild action exists to recover from.
func (c *catalog) fileStamps() (map[string]fileStamp, error) {
	stamps := make(map[string]fileStamp)
	if c == nil {
		return stamps, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil {
		return stamps, nil
	}
	rows, err := c.db.Query(`SELECT path, size, mtime FROM instances`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var st fileStamp
		if err := rows.Scan(&path, &st.size, &st.mtime); err != nil {
			return nil, err
		}
		stamps[path] = st
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return stamps, nil
}

// upsertMetas indexes files that have already been parsed, returning how many
// rows were written.
//
// The counterpart to ingestPaths, which parses the files itself and serially.
// A scan has already parsed its changed files, on a worker pool; handing it
// paths instead of results would throw that work away and do it again. Both
// exist because the callers genuinely differ: the retrieve and import hooks
// hold only paths, a scan holds results.
func (c *catalog) upsertMetas(metas []fileMeta) int {
	if c == nil || len(metas) == 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil {
		return 0
	}
	tx, err := c.db.Begin()
	if err != nil {
		return 0
	}
	defer tx.Rollback()
	n := 0
	for _, m := range metas {
		if upsertMeta(tx, m) == nil {
			n++
		}
	}
	if tx.Commit() != nil {
		return 0
	}
	return n
}

// ingestPaths parses the given DICOM files and upserts them into the index.
// Files that fail to parse are skipped. Returns the number of files indexed.
// Parsing happens before the catalog lock is taken so long ingests do not
// block concurrent reads.
func (c *catalog) ingestPaths(paths []string) int {
	if c == nil || len(paths) == 0 {
		return 0
	}
	var metas []fileMeta
	for _, p := range paths {
		if m, ok := parseLocalFileMeta(p); ok {
			metas = append(metas, m)
		}
	}
	if len(metas) == 0 {
		return 0
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil {
		return 0
	}
	tx, err := c.db.Begin()
	if err != nil {
		return 0
	}
	defer tx.Rollback()
	n := 0
	for _, m := range metas {
		if upsertMeta(tx, m) == nil {
			n++
		}
	}
	if tx.Commit() != nil {
		return 0
	}
	return n
}

// removePaths deletes the given file paths from the index, then prunes any
// series, studies, and patients left without children. Returns the number of
// instance rows removed.
func (c *catalog) removePaths(paths []string) int {
	if c == nil || len(paths) == 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil {
		return 0
	}
	tx, err := c.db.Begin()
	if err != nil {
		return 0
	}
	defer tx.Rollback()
	n := 0
	for _, p := range paths {
		res, execErr := tx.Exec(`DELETE FROM instances WHERE path = ?`, p)
		if execErr == nil {
			if k, _ := res.RowsAffected(); k > 0 {
				n += int(k)
			}
		}
	}
	for _, stmt := range []string{
		`DELETE FROM series   WHERE NOT EXISTS (SELECT 1 FROM instances i WHERE i.series_uid = series.series_uid)`,
		`DELETE FROM studies  WHERE NOT EXISTS (SELECT 1 FROM series   s WHERE s.study_uid  = studies.study_uid)`,
		`DELETE FROM patients WHERE NOT EXISTS (SELECT 1 FROM studies st WHERE st.patient_id = patients.patient_id)`,
	} {
		if _, execErr := tx.Exec(stmt); execErr != nil {
			return 0
		}
	}
	if tx.Commit() != nil {
		return 0
	}
	return n
}

// load returns the indexed hierarchy in the same shapes scanLocalFolder
// produces, so the tree-population code is shared between disk scans and
// index loads.
func (c *catalog) load() ([]localStudy, []localSeries, map[string][]string, error) {
	empty := make(map[string][]string)
	if c == nil {
		return nil, nil, empty, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil {
		return nil, nil, empty, nil
	}

	var studies []localStudy
	rows, err := c.db.Query(`
		SELECT st.study_uid, st.patient_id, p.patient_name, st.study_date, st.study_desc, st.accession, st.modalities
		FROM studies st JOIN patients p ON p.patient_id = st.patient_id`)
	if err != nil {
		return nil, nil, empty, err
	}
	for rows.Next() {
		var s localStudy
		if err := rows.Scan(&s.studyUID, &s.patientID, &s.patientName,
			&s.studyDate, &s.studyDesc, &s.accession, &s.modalities); err != nil {
			rows.Close()
			return nil, nil, empty, err
		}
		studies = append(studies, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, empty, err
	}

	var series []localSeries
	rows, err = c.db.Query(`
		SELECT se.series_uid, se.study_uid, se.modality, se.series_number, se.series_desc, COUNT(i.path)
		FROM series se LEFT JOIN instances i ON i.series_uid = se.series_uid
		GROUP BY se.series_uid`)
	if err != nil {
		return nil, nil, empty, err
	}
	for rows.Next() {
		var sr localSeries
		if err := rows.Scan(&sr.seriesUID, &sr.studyUID, &sr.modality,
			&sr.seriesNumber, &sr.seriesDesc, &sr.numInstances); err != nil {
			rows.Close()
			return nil, nil, empty, err
		}
		series = append(series, sr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, empty, err
	}

	filesByUID := make(map[string][]string)
	rows, err = c.db.Query(`SELECT series_uid, path FROM instances ORDER BY path`)
	if err != nil {
		return nil, nil, empty, err
	}
	for rows.Next() {
		var uid, path string
		if err := rows.Scan(&uid, &path); err != nil {
			rows.Close()
			return nil, nil, empty, err
		}
		filesByUID[uid] = append(filesByUID[uid], path)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, empty, err
	}

	return studies, series, filesByUID, nil
}
