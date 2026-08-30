package store

import "testing"

func TestPermReconGlobalFallback(t *testing.T) {
	st, err := Open(t.TempDir() + "/t.db")
	if err != nil { t.Fatal(err) }
	defer st.Close()

	// default deny for any elType
	if v, _ := st.ReconAccess("fresh", "TEACHER"); v { t.Fatal("default recon should be denied") }
	if v, _ := st.ReconAccess("fresh", "ROOM"); v { t.Fatal("default recon should be denied") }

	// global on -> fresh user inherits (elType ignored; single recon flag)
	if err := st.SetReconType("TEACHER", true); err != nil { t.Fatal(err) }
	if v, _ := st.ReconAccess("fresh", "ROOM"); !v { t.Fatal("fresh should inherit global recon") }

	// per-user deny override beats global
	if err := st.SetReconOverride("fresh", "TEACHER", false); err != nil { t.Fatal(err) }
	if v, _ := st.ReconAccess("fresh", "TEACHER"); v { t.Fatal("per-user deny should beat global recon") }

	// clear overrides -> back to global
	if _, err := st.ClearReconOverrides("fresh"); err != nil { t.Fatal(err) }
	if v, _ := st.ReconAccess("fresh", "SUBJECT"); !v { t.Fatal("after clear, fresh should inherit global recon again") }

	// per-user grant beats global off
	if err := st.SetReconType("TEACHER", false); err != nil { t.Fatal(err) }
	if err := st.SetReconOverride("fresh", "TEACHER", true); err != nil { t.Fatal(err) }
	if v, _ := st.ReconAccess("fresh", "ROOM"); !v { t.Fatal("per-user grant should beat global off") }

	// RevokeAll -> everything off
	if _, err := st.RevokeAll(); err != nil { t.Fatal(err) }
	for _, ty := range []string{"TEACHER", "ROOM", "SUBJECT"} {
		if v, _ := st.ReconAccess("fresh", ty); v { t.Fatalf("after RevokeAll, recon should be denied") }
	}
}

func TestReconBoostedMutualExclusion(t *testing.T) {
	st, err := Open(t.TempDir() + "/t.db")
	if err != nil { t.Fatal(err) }
	defer st.Close()

	// Grant boosted -> recon revoked
	if err := st.SetBoostedFlag("alice", true); err != nil { t.Fatal(err) }
	if in, err := st.BoostedAccess("alice"); err != nil || !in { t.Fatal("boosted should grant boosted access") }
	if v, _ := st.ReconAccess("alice", "TEACHER"); v { t.Fatal("boosting should revoke recon") }

	// Grant recon -> boosted revoked
	if err := st.SetReconOverride("alice", "TEACHER", true); err != nil { t.Fatal(err) }
	if v, _ := st.ReconAccess("alice", "TEACHER"); !v { t.Fatal("recon should be granted") }
	if in, err := st.BoostedAccess("alice"); err != nil || in { t.Fatal("recon should revoke boosted") }

	// Revoking boosted restores nothing automatically, but revoking recon
	// leaves the user free to be boosted again.
	if err := st.SetReconOverride("alice", "TEACHER", false); err != nil { t.Fatal(err) }
	if err := st.SetBoostedFlag("alice", true); err != nil { t.Fatal(err) }
	if in, err := st.BoostedAccess("alice"); err != nil || !in { t.Fatal("re-granting boosted after clearing recon should work") }
}
