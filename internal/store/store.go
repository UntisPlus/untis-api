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
	return &Store{db: db}, nil
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

// FeatureEnabled reports whether a user is allowed to use a named feature.
// Default is deny: access must be granted explicitly.
func (s *Store) FeatureEnabled(username, feature string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM perms WHERE username=? AND feature=? AND allowed=1`,
		username, feature).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetFeature grants or revokes a named feature for a user.
func (s *Store) SetFeature(username, feature string, allowed bool) error {
	v := 0
	if allowed {
		v = 1
	}
	_, err := s.db.Exec(`INSERT INTO perms (username, feature, allowed) VALUES (?,?,?)
		ON CONFLICT(username, feature) DO UPDATE SET allowed=excluded.allowed`,
		username, feature, v)
	return err
}

// RevokeAll removes all grants for a feature across every user.
func (s *Store) RevokeAll() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM perms WHERE feature=? AND allowed=1`, "recon")
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
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
	rows, err := s.db.Query(`SELECT DISTINCT class_id, class_name FROM users WHERE class_id > 0`)
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
