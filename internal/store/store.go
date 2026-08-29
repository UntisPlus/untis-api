package store

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite"
)

type User struct {
	ID          int64
	Username    string
	Password    string
	Method      string // "password" or "key"
	PersonID    int64
	PersonType  int64
	ClassID     int64
	ClassName   string
	Email       string
	DisplayName string
	CreatedAt   time.Time
	LastSeen    time.Time
}

type Class struct {
	ID   int64
	Name string
}

// ClassToken binds an opaque calendar subscription token to a school and
// either a class (ClassID > 0) or a personal student timetable (PersonID > 0).
type ClassToken struct {
	Token      string
	School     string
	ClassID    int64
	PersonID   int64
	CreatedAt  int64
	LastAccess int64
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL UNIQUE,
		password TEXT NOT NULL DEFAULT '',
		method TEXT NOT NULL DEFAULT 'password',
		person_id INTEGER NOT NULL DEFAULT 0,
		person_type INTEGER NOT NULL DEFAULT 0,
		class_id INTEGER NOT NULL DEFAULT 0,
		class_name TEXT NOT NULL DEFAULT '',
		email TEXT NOT NULL DEFAULT '',
		display_name TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL DEFAULT 0,
		last_seen INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS secrets (
		username TEXT NOT NULL PRIMARY KEY,
		secret TEXT NOT NULL,
		updated_at INTEGER NOT NULL
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS perms (
		username TEXT NOT NULL,
		feature TEXT NOT NULL,
		allowed INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (username, feature)
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS recon_elements (
		el_type TEXT NOT NULL,
		el_id INTEGER NOT NULL,
		PRIMARY KEY (el_type, el_id)
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS recon_scan (
		school TEXT NOT NULL,
		class_id INTEGER NOT NULL,
		scan_until TEXT NOT NULL,
		PRIMARY KEY (school, class_id)
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS class_tokens (
		token TEXT NOT NULL PRIMARY KEY,
		school TEXT NOT NULL,
		class_id INTEGER NOT NULL DEFAULT 0,
		person_id INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		last_access INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS timetable_versions (
		school TEXT NOT NULL,
		class_id INTEGER NOT NULL,
		version INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (school, class_id)
	)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS timetable_changes (
		school TEXT NOT NULL,
		class_id INTEGER NOT NULL,
		period_id INTEGER NOT NULL,
		kind TEXT NOT NULL DEFAULT 'ADDED',
		start TEXT NOT NULL DEFAULT '',
		end TEXT NOT NULL DEFAULT '',
		subject TEXT NOT NULL DEFAULT '',
		room TEXT NOT NULL DEFAULT '',
		description TEXT NOT NULL DEFAULT '',
		mod_ver INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (school, class_id, period_id)
	)`)
	if err != nil {
		return nil, err
	}
	// migration: add person_id to class_tokens for older databases
	if err := addColumnIfMissing(db, "class_tokens", "person_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// addColumnIfMissing adds a column to a table if it does not already exist.
func addColumnIfMissing(db *sql.DB, table, column, ddl string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + ddl)
	return err //nolint
}

func (s *Store) UpsertSecret(username, secret string) error {
	_, err := s.db.Exec(`INSERT INTO secrets (username, secret, updated_at) VALUES (?,?,?)
		ON CONFLICT(username) DO UPDATE SET secret=excluded.secret, updated_at=excluded.updated_at`,
		username, secret, time.Now().Unix())
	return err
}

func (s *Store) GetSecret(username string) (string, error) {
	var sec string
	err := s.db.QueryRow(`SELECT secret FROM secrets WHERE username=?`, username).Scan(&sec)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return sec, err
}

// globalPermUser is the special perms row username holding the global switch
// for a reconstruction element type. Per-user rows override it.
const globalPermUser = "*"

// Permission feature names.
const (
	FeatureReconRoom   = "ROOM"
	FeatureReconTeach  = "TEACHER"
	FeatureReconSubj   = "SUBJECT"
	FeatureGodAPI      = "god-api"
	FeatureGodEditor   = "god-api-editor"
)

// ReconTypes is the set of reconstruction element types grantable per user
// (ROOM/TEACHER/SUBJECT). The god features are separate and mutually exclusive
// with these.
var ReconTypes = []string{FeatureReconRoom, FeatureReconTeach, FeatureReconSubj}

// GodFeatures is the set of privileged per-user permission features.
var GodFeatures = []string{FeatureGodAPI, FeatureGodEditor}

// HasPerm reports whether a user has an explicit per-user permission row set to
// allowed. Global switches do not count.
func (s *Store) HasPerm(username, feature string) (bool, error) {
	var v int
	row := s.db.QueryRow(`SELECT allowed FROM perms WHERE username=? AND feature=?`, username, feature)
	if err := row.Scan(&v); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return v != 0, nil
}

// ReconAccess reports whether a user may use a reconstruction element type
// (one of TEACHER/ROOM/SUBJECT). A per-user override wins if one exists;
// otherwise the global switch for the type applies; default is deny.
func (s *Store) ReconAccess(username, elType string) (bool, error) {
	var v int
	row := s.db.QueryRow(`SELECT allowed FROM perms WHERE username=? AND feature=?`, username, elType)
	if err := row.Scan(&v); err == nil {
		return v != 0, nil
	}
	row = s.db.QueryRow(`SELECT allowed FROM perms WHERE username=? AND feature=?`, globalPermUser, elType)
	if err := row.Scan(&v); err == nil {
		return v != 0, nil
	}
	return false, nil
}

// SetReconType sets the global switch for an element type across all users.
func (s *Store) SetReconType(elType string, allowed bool) error {
	return s.setPerm(globalPermUser, elType, allowed)
}

// SetReconOverride grants or revokes a reconstruction element type for a
// single user, taking precedence over the global switch.
func (s *Store) SetReconOverride(username, elType string, allowed bool) error {
	return s.setPerm(username, elType, allowed)
}

func (s *Store) setPerm(username, elType string, allowed bool) error {
	// god-api and god-api-editor are mutually exclusive with the
	// reconstruction types (and vice versa): granting one side revokes the
	// other for the same user. The global switch ("*") is not a real user,
	// so exclusion only applies to per-user overrides.
	if username != globalPermUser && allowed {
		if elType == FeatureGodAPI || elType == FeatureGodEditor {
			for _, t := range ReconTypes {
				_ = s.setPermFalse(username, t)
			}
			_ = s.setPermFalse(username, FeatureGodAPI)
			_ = s.setPermFalse(username, FeatureGodEditor)
		} else {
			for _, t := range ReconTypes {
				if t == elType {
					continue
				}
				_ = s.setPermFalse(username, t)
			}
			_ = s.setPermFalse(username, FeatureGodAPI)
			_ = s.setPermFalse(username, FeatureGodEditor)
		}
	}
	v := 0
	if allowed {
		v = 1
	}
	_, err := s.db.Exec(`INSERT INTO perms (username, feature, allowed) VALUES (?,?,?)
		ON CONFLICT(username, feature) DO UPDATE SET allowed=excluded.allowed`,
		username, elType, v)
	return err
}

func (s *Store) setPermFalse(username, feature string) error {
	_, err := s.db.Exec(`INSERT INTO perms (username, feature, allowed) VALUES (?,?,0)
		ON CONFLICT(username, feature) DO UPDATE SET allowed=0`, username, feature)
	return err
}

// ClearReconOverrides removes any per-user reconstruction overrides for a
// username so they fall back to the global switches again.
func (s *Store) ClearReconOverrides(username string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM perms WHERE username=? AND username<>?`,
		username, globalPermUser)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RevokeAll clears every permission row (global switches and per-user
// overrides), returning every user to the default state of class-pool-only.
func (s *Store) RevokeAll() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM perms`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PermRow is a single row in the perms table.
type PermRow struct {
	Username string
	Feature  string
	Allowed  bool
}

// AllPerms returns every permission row, global switches (username "*")
// first, then per-user overrides.
func (s *Store) AllPerms() ([]PermRow, error) {
	rows, err := s.db.Query(`SELECT username, feature, allowed FROM perms
		ORDER BY username='*' DESC, username, feature`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PermRow
	for rows.Next() {
		var p PermRow
		var a int
		if err := rows.Scan(&p.Username, &p.Feature, &a); err != nil {
			return nil, err
		}
		p.Allowed = a != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// SaveReconElements replaces the persisted teacher/room/subject set with the
// given types->id map. It is written on shutdown so a later boot can answer
// reconstruction requests before the background scan revalidates.
func (s *Store) SaveReconElements(elems map[string][]int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM recon_elements`); err != nil {
		tx.Rollback()
		return err
	}
	ins, err := tx.Prepare(`INSERT INTO recon_elements (el_type, el_id) VALUES (?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for t, ids := range elems {
		for _, id := range ids {
			if _, err := ins.Exec(t, id); err != nil {
				ins.Close()
				tx.Rollback()
				return err
			}
		}
	}
	ins.Close()
	return tx.Commit()
}

// LoadReconElements returns the persisted teacher/room/subject set as
// type -> [ids].
func (s *Store) LoadReconElements() (map[string][]int64, error) {
	rows, err := s.db.Query(`SELECT el_type, el_id FROM recon_elements`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]int64{}
	for rows.Next() {
		var t string
		var id int64
		if err := rows.Scan(&t, &id); err != nil {
			return nil, err
		}
		out[t] = append(out[t], id)
	}
	return out, rows.Err()
}

// SaveReconScanAt records how far a class's recon scan reached. On boot this is
// cleared so the scan re-runs from the year start for freshness.
func (s *Store) SaveReconScanAt(school string, classID int64, until string) error {
	_, err := s.db.Exec(`INSERT INTO recon_scan (school, class_id, scan_until) VALUES (?,?,?)
		ON CONFLICT(school, class_id) DO UPDATE SET scan_until=excluded.scan_until`,
		school, classID, until)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) UpsertUser(u *User) error {
	now := time.Now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	_, err := s.db.Exec(`INSERT INTO users
		(username, password, method, person_id, person_type, class_id, class_name, email, display_name, created_at, last_seen)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(username) DO UPDATE SET
			password=excluded.password,
			method=excluded.method,
			person_id=excluded.person_id,
			person_type=excluded.person_type,
			class_id=excluded.class_id,
			class_name=excluded.class_name,
			email=excluded.email,
			display_name=excluded.display_name,
			last_seen=excluded.last_seen`,
		u.Username, u.Password, u.Method, u.PersonID, u.PersonType,
		u.ClassID, u.ClassName, u.Email, u.DisplayName,
		u.CreatedAt.Unix(), now.Unix())
	return err
}

func (s *Store) Touch(username string) error {
	_, err := s.db.Exec(`UPDATE users SET last_seen=? WHERE username=?`, time.Now().Unix(), username)
	return err
}

func (s *Store) GetUser(username string) (*User, error) {
	return s.scanUser(s.db.QueryRow(
		`SELECT id,username,password,method,person_id,person_type,class_id,class_name,email,display_name,created_at,last_seen FROM users WHERE username=?`,
		username))
}

// UserByPersonID returns a user with the given person id, preferring the most
// recently active one.
func (s *Store) UserByPersonID(personID int64) (*User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT id,username,password,method,person_id,person_type,class_id,class_name,email,display_name,created_at,last_seen
		FROM users WHERE person_id=? ORDER BY last_seen DESC, id DESC LIMIT 1`, personID))
}

func (s *Store) Pool() ([]Class, error) {
	rows, err := s.db.Query(`SELECT class_id, COALESCE(MAX(class_name),'') AS name
		FROM users WHERE class_id > 0 GROUP BY class_id ORDER BY class_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Class
	for rows.Next() {
		var c Class
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) UserCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) PoolContains(classID int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE class_id=?`, classID).Scan(&n)
	return n > 0, err
}

// OwnerForClass returns the most recently active user in the given class,
// preferring accounts that can be replayed (password or key with secret).
func (s *Store) OwnerForClass(classID int64) (*User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT id,username,password,method,person_id,person_type,class_id,class_name,email,display_name,created_at,last_seen
		FROM users WHERE class_id=? AND password<>'' ORDER BY last_seen DESC, id DESC LIMIT 1`, classID))
}

func (s *Store) AnyUser() (*User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT id,username,password,method,person_id,person_type,class_id,class_name,email,display_name,created_at,last_seen
		FROM users ORDER BY last_seen DESC, id DESC LIMIT 1`))
}

// ListUsers returns every account, most recently seen first.
func (s *Store) ListUsers() ([]*User, error) {
	rows, err := s.db.Query(`SELECT id,username,password,method,person_id,person_type,class_id,class_name,email,display_name,created_at,last_seen
		FROM users ORDER BY last_seen DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := s.scanUserRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteUser removes an account plus its secret, per-user permission overrides
// and any personal calendar tokens bound to that person. Global switches are
// untouched.
func (s *Store) DeleteUser(username string) (int64, error) {
	u, err := s.GetUser(username)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	n := int64(0)
	var del func(q string, args ...any) error
	del = func(q string, args ...any) error {
		res, err := tx.Exec(q, args...)
		if err != nil {
			return err
		}
		c, _ := res.RowsAffected()
		n += c
		return nil
	}
	if err := del(`DELETE FROM users WHERE username=?`, username); err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := del(`DELETE FROM secrets WHERE username=?`, username); err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := del(`DELETE FROM perms WHERE username=? AND username<>?`, username, globalPermUser); err != nil {
		tx.Rollback()
		return 0, err
	}
	if u != nil && u.PersonID > 0 {
		if err := del(`DELETE FROM class_tokens WHERE person_id=? AND class_id=0`, u.PersonID); err != nil {
			tx.Rollback()
			return 0, err
		}
	}
	return n, tx.Commit()
}

func (s *Store) scanUser(row *sql.Row) (*User, error) {
	var u User
	var ca, ls int64
	err := row.Scan(&u.ID, &u.Username, &u.Password, &u.Method, &u.PersonID, &u.PersonType,
		&u.ClassID, &u.ClassName, &u.Email, &u.DisplayName, &ca, &ls)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt = time.Unix(ca, 0)
	u.LastSeen = time.Unix(ls, 0)
	return &u, nil
}

// scanUserRow is the *sql.Rows equivalent of scanUser.
func (s *Store) scanUserRow(row *sql.Rows) (*User, error) {
	var u User
	var ca, ls int64
	err := row.Scan(&u.ID, &u.Username, &u.Password, &u.Method, &u.PersonID, &u.PersonType,
		&u.ClassID, &u.ClassName, &u.Email, &u.DisplayName, &ca, &ls)
	if err != nil {
		return nil, err
	}
	u.CreatedAt = time.Unix(ca, 0)
	u.LastSeen = time.Unix(ls, 0)
	return &u, nil
}

func (s *Store) CreateClassToken(t *ClassToken) error {
	_, err := s.db.Exec(`INSERT INTO class_tokens (token, school, class_id, person_id, created_at, last_access)
		VALUES (?,?,?,?,?,?)`, t.Token, t.School, t.ClassID, t.PersonID, t.CreatedAt, t.LastAccess)
	return err
}

// ClassTokenForClass returns the existing token for a school+class, if any.
func (s *Store) ClassTokenForClass(school string, classID int64) (*ClassToken, error) {
	return s.scanClassToken(s.db.QueryRow(`SELECT token, school, class_id, person_id, created_at, last_access
		FROM class_tokens WHERE school=? AND class_id=?`, school, classID))
}

// ClassTokenForPerson returns the existing personal token for a school+person.
func (s *Store) ClassTokenForPerson(school string, personID int64) (*ClassToken, error) {
	return s.scanClassToken(s.db.QueryRow(`SELECT token, school, class_id, person_id, created_at, last_access
		FROM class_tokens WHERE school=? AND person_id=?`, school, personID))
}

func (s *Store) ClassTokenByToken(token string) (*ClassToken, error) {
	return s.scanClassToken(s.db.QueryRow(`SELECT token, school, class_id, person_id, created_at, last_access
		FROM class_tokens WHERE token=?`, token))
}

func (s *Store) scanClassToken(row *sql.Row) (*ClassToken, error) {
	var t ClassToken
	err := row.Scan(&t.Token, &t.School, &t.ClassID, &t.PersonID, &t.CreatedAt, &t.LastAccess)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListClassTokens returns every calendar subscription token, newest first.
func (s *Store) ListClassTokens() ([]*ClassToken, error) {
	rows, err := s.db.Query(`SELECT token, school, class_id, person_id, created_at, last_access
		FROM class_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ClassToken
	for rows.Next() {
		var t ClassToken
		if err := rows.Scan(&t.Token, &t.School, &t.ClassID, &t.PersonID, &t.CreatedAt, &t.LastAccess); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// DeleteClassToken removes a calendar subscription token by its value.
func (s *Store) DeleteClassToken(token string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM class_tokens WHERE token=?`, token)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// TouchClassToken updates the last-access timestamp of an existing token.
func (s *Store) TouchClassToken(token string, at int64) error {
	_, err := s.db.Exec(`UPDATE class_tokens SET last_access=? WHERE token=?`, at, token)
	return err
}

// PeriodRow is the persistent snapshot of one period used for change detection.
type PeriodRow struct {
	PeriodID    int64
	Kind        string // ADDED, CHANGED or REMOVED
	Start       string
	End         string
	Subject     string
	Room        string
	Description string
	ModVer      int64
}

func (s *Store) ClassVersion(school string, classID int64) int64 {
	var v int64
	_ = s.db.QueryRow(`SELECT version FROM timetable_versions WHERE school=? AND class_id=?`, school, classID).Scan(&v)
	return v
}

// LoadClassSnapshot returns the latest known periods for a class.
func (s *Store) LoadClassSnapshot(school string, classID int64) ([]PeriodRow, error) {
	rows, err := s.db.Query(`SELECT period_id, kind, start, end, subject, room, description, mod_ver
		FROM timetable_changes WHERE school=? AND class_id=? ORDER BY period_id`, school, classID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PeriodRow
	for rows.Next() {
		var r PeriodRow
		if err := rows.Scan(&r.PeriodID, &r.Kind, &r.Start, &r.End, &r.Subject, &r.Room, &r.Description, &r.ModVer); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReplaceClassSnapshot transactionally rewrites the snapshot for a class. It
// returns the updated period rows (with new mod versions bumped) and the number
// of rows whose mod version changed. Removed periods whose start date is before
// dropRemovedBefore (format YYYY-MM-DD) are silently deleted rather than being
// reported, so periods that merely age out of the sliding fetch window do not
// trigger spurious REMOVED notifications.
func (s *Store) ReplaceClassSnapshot(school string, classID int64, next []PeriodRow, newVer int64, dropRemovedBefore string) (int, error) {
	old, err := s.LoadClassSnapshot(school, classID)
	if err != nil {
		return 0, err
	}
	oldByID := map[int64]PeriodRow{}
	for _, r := range old {
		oldByID[r.PeriodID] = r
	}
	changed := 0
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM timetable_changes WHERE school=? AND class_id=?`, school, classID); err != nil {
		return 0, err
	}
	for _, r := range next {
		prev, existed := oldByID[r.PeriodID]
		cur := r
		if existed && prev.Start == r.Start && prev.End == r.End &&
			prev.Subject == r.Subject && prev.Room == r.Room && prev.Description == r.Description {
			cur.Kind = "UNCHANGED"
			cur.ModVer = prev.ModVer
		} else {
			cur.ModVer = newVer
			if existed {
				cur.Kind = "CHANGED"
			} else {
				cur.Kind = "ADDED"
			}
			changed++
		}
		oldByID[r.PeriodID] = cur
		if cur.Kind == "UNCHANGED" {
			// keep existing row; preserve old values
			cur.Start, cur.End, cur.Subject, cur.Room, cur.Description = prev.Start, prev.End, prev.Subject, prev.Room, prev.Description
			rows, err := tx.Exec(`INSERT INTO timetable_changes (school,class_id,period_id,kind,start,end,subject,room,description,mod_ver)
				VALUES (?,?,?,?,?,?,?,?,?,?)`, school, classID, r.PeriodID, cur.Kind, cur.Start, cur.End, cur.Subject, cur.Room, cur.Description, cur.ModVer)
			if err != nil {
				return 0, err
			}
			_ = rows
			continue
		}
		if _, err := tx.Exec(`INSERT INTO timetable_changes (school,class_id,period_id,kind,start,end,subject,room,description,mod_ver)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, school, classID, cur.PeriodID, cur.Kind, cur.Start, cur.End, cur.Subject, cur.Room, cur.Description, cur.ModVer); err != nil {
			return 0, err
		}
	}
	// mark periods present in old but absent from new as REMOVED
	seen := map[int64]bool{}
	for _, r := range next {
		seen[r.PeriodID] = true
	}
	for pid, r := range oldByID {
		if seen[pid] {
			continue
		}
		// silently drop past periods that left the window
		if dropRemovedBefore != "" && len(r.Start) >= 10 && r.Start[:10] < dropRemovedBefore {
			continue
		}
		r.Kind = "REMOVED"
		r.ModVer = newVer
		if _, err := tx.Exec(`INSERT INTO timetable_changes (school,class_id,period_id,kind,start,end,subject,room,description,mod_ver)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, school, classID, pid, r.Kind, r.Start, r.End, r.Subject, r.Room, r.Description, r.ModVer); err != nil {
			return 0, err
		}
		changed++
	}
	if changed > 0 {
		if _, err := tx.Exec(`INSERT INTO timetable_versions (school,class_id,version) VALUES (?,?,?)
			ON CONFLICT(school,class_id) DO UPDATE SET version=excluded.version`, school, classID, newVer); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changed, nil
}

// PendingChanges returns the periods modified after the given version, plus the
// current class version.
func (s *Store) PendingChanges(school string, classID int64, since int64) ([]PeriodRow, int64, error) {
	rows, err := s.db.Query(`SELECT period_id, kind, start, end, subject, room, description, mod_ver
		FROM timetable_changes WHERE school=? AND class_id=? AND mod_ver>?
		ORDER BY period_id`, school, classID, since)
	if err != nil {
		return nil, s.ClassVersion(school, classID), err
	}
	defer rows.Close()
	var out []PeriodRow
	for rows.Next() {
		var r PeriodRow
		if err := rows.Scan(&r.PeriodID, &r.Kind, &r.Start, &r.End, &r.Subject, &r.Room, &r.Description, &r.ModVer); err != nil {
			return nil, s.ClassVersion(school, classID), err
		}
		out = append(out, r)
	}
	return out, s.ClassVersion(school, classID), rows.Err()
}
