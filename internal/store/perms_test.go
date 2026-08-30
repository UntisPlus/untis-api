package store

import (
	"strings"
	"testing"
	"time"
)

func TestPermReconGlobalFallback(t *testing.T) {
	st, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// default deny for any elType
	if v, _ := st.ReconAccess("fresh", "TEACHER"); v {
		t.Fatal("default recon should be denied")
	}
	if v, _ := st.ReconAccess("fresh", "ROOM"); v {
		t.Fatal("default recon should be denied")
	}

	// global on -> fresh user inherits (elType ignored; single recon flag)
	if err := st.SetReconType("TEACHER", true); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.ReconAccess("fresh", "ROOM"); !v {
		t.Fatal("fresh should inherit global recon")
	}

	// per-user deny override beats global
	if err := st.SetReconOverride("fresh", "TEACHER", false); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.ReconAccess("fresh", "TEACHER"); v {
		t.Fatal("per-user deny should beat global recon")
	}

	// clear overrides -> back to global
	if _, err := st.ClearReconOverrides("fresh"); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.ReconAccess("fresh", "SUBJECT"); !v {
		t.Fatal("after clear, fresh should inherit global recon again")
	}

	// per-user grant beats global off
	if err := st.SetReconType("TEACHER", false); err != nil {
		t.Fatal(err)
	}
	if err := st.SetReconOverride("fresh", "TEACHER", true); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.ReconAccess("fresh", "ROOM"); !v {
		t.Fatal("per-user grant should beat global off")
	}

	// RevokeAll -> everything off
	if _, err := st.RevokeAll(); err != nil {
		t.Fatal(err)
	}
	for _, ty := range []string{"TEACHER", "ROOM", "SUBJECT"} {
		if v, _ := st.ReconAccess("fresh", ty); v {
			t.Fatalf("after RevokeAll, recon should be denied")
		}
	}
}

func TestReconBoostedMutualExclusion(t *testing.T) {
	st, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Grant boosted -> recon revoked
	if err := st.SetBoostedFlag("alice", true); err != nil {
		t.Fatal(err)
	}
	if in, err := st.BoostedAccess("alice"); err != nil || !in {
		t.Fatal("boosted should grant boosted access")
	}
	if v, _ := st.ReconAccess("alice", "TEACHER"); v {
		t.Fatal("boosting should revoke recon")
	}

	// Grant recon -> boosted revoked
	if err := st.SetReconOverride("alice", "TEACHER", true); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.ReconAccess("alice", "TEACHER"); !v {
		t.Fatal("recon should be granted")
	}
	if in, err := st.BoostedAccess("alice"); err != nil || in {
		t.Fatal("recon should revoke boosted")
	}

	// Revoking boosted restores nothing automatically, but revoking recon
	// leaves the user free to be boosted again.
	if err := st.SetReconOverride("alice", "TEACHER", false); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBoostedFlag("alice", true); err != nil {
		t.Fatal(err)
	}
	if in, err := st.BoostedAccess("alice"); err != nil || !in {
		t.Fatal("re-granting boosted after clearing recon should work")
	}
}

func TestUsernameCaseInsensitiveMerge(t *testing.T) {
	path := t.TempDir() + "/t.db"
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate legacy rows whose usernames differ only by case.
	now := time.Now().Unix()
	ins := func(name, pw string, ls int64) {
		if _, err := st.db.Exec(`INSERT INTO users
			(username,password,method,person_id,person_type,class_id,class_name,created_at,last_seen)
			VALUES (?,?,?,?,?,?,?,?,?)`, name, pw, "key", 1, 5, 4419, "10aR", now, ls); err != nil {
			t.Fatal(err)
		}
	}
	ins("LaugrÜ", "oldpw", now-1000)
	ins("laugrÜ", "newpw", now)
	if _, err := st.db.Exec(`INSERT INTO secrets (username, secret, updated_at) VALUES (?,?,?)`,
		"LaugrÜ", "OLDSECRET", now-1000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO secrets (username, secret, updated_at) VALUES (?,?,?)`,
		"laugrÜ", "NEWSECRET", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO perms (username, feature, allowed) VALUES (?,?,?)`,
		"LaugrÜ", FeatureBoosted, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO perms (username, feature, allowed) VALUES (?,?,?)`,
		"laugrÜ", FeatureRecon, 1); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Reopen runs the merge migration.
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	u, err := st.GetUser("LaugrÜ")
	if err != nil || u == nil {
		t.Fatalf("GetUser(capital) err=%v u=%v", err, u)
	}
	if u.Password != "newpw" {
		t.Fatal("should keep the most recently active variant's credentials")
	}
	if u.Username != strings.ToLower(u.Username) {
		t.Fatal("canonical username should be lowercase")
	}
	if u2, _ := st.GetUser("laugrÜ"); u2 == nil || u2.ID != u.ID {
		t.Fatal("both casings must resolve to the same account")
	}
	if sec, _ := st.GetSecret("LAUGRÜ"); sec != "NEWSECRET" {
		t.Fatalf("secret should be merged and case-insensitive, got %q", sec)
	}
	if in, err := st.BoostedAccess("laugrÜ"); err != nil || !in {
		t.Fatal("merged account should be boosted regardless of case")
	}
	if v, _ := st.ReconAccess("LAUGRÜ", "TEACHER"); v {
		t.Fatal("boost/recon conflict should resolve in favour of boosted (recon off)")
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("duplicates must collapse to a single row, got %d", n)
	}
}

func TestUsernameCaseInsensitiveWrites(t *testing.T) {
	st, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.SetBoostedFlag("BoBbY", true); err != nil {
		t.Fatal(err)
	}
	if in, _ := st.BoostedAccess("bobby"); !in {
		t.Fatal("grant+lookup must be case-insensitive")
	}
	if in, _ := st.HasPerm("BOBBY", FeatureBoosted); !in {
		t.Fatal("HasPerm must be case-insensitive")
	}

	u := &User{Username: "EvAn", Password: "p", Method: "key", PersonType: 5, ClassID: 10}
	if err := st.UpsertUser(u); err != nil {
		t.Fatal(err)
	}
	if err := st.Touch("evan"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetUser("evan")
	if err != nil || got == nil {
		t.Fatalf("GetUser err=%v user=%v", err, got)
	}
	got2, err := st.GetUser("EVAN")
	if err != nil || got2 == nil {
		t.Fatalf("GetUser(caps) err=%v user=%v", err, got2)
	}
	if got.ID != got2.ID {
		t.Fatal("upsert+lookup must hit the same account regardless of case")
	}
}

func TestUsernameCaseSelfHealOnWrite(t *testing.T) {
	path := t.TempDir() + "/t.db"
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	// A capital-cased account exists (as if seeded before normalization) with a
	// permission flag stored under the same capital casing.
	if _, err := st.db.Exec(`INSERT INTO users (username,password,method,last_seen) VALUES (?,?,?,?)`,
		"Evadee", "pw", "key", 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO perms (username, feature, allowed) VALUES (?,?,?)`,
		"Evadee", FeatureBoosted, 1); err != nil {
		t.Fatal(err)
	}

	// The app now logs in with the lowercase casing: this must collapse the
	// variants at write time (not wait for a restart) and keep the flag.
	u := &User{Username: "evadee", Password: "pw", Method: "key", PersonType: 5, ClassID: 4419}
	if err := st.UpsertUser(u); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetUser("Evadee")
	if err != nil || got == nil {
		t.Fatalf("GetUser err=%v user=%v", err, got)
	}
	got2, _ := st.GetUser("evadee")
	if got2 == nil || got2.ID != got.ID {
		t.Fatal("case variants must collapse to one account")
	}
	if in, _ := st.BoostedAccess("evadee"); !in {
		t.Fatal("flag must survive the write-time merge")
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected a single merged row, got %d", n)
	}
}
