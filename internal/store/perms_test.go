package store

import "testing"

func TestPermGlobalFallback(t *testing.T) {
	st, err := Open(t.TempDir() + "/t.db")
	if err != nil { t.Fatal(err) }
	defer st.Close()

	// default deny
	if v, _ := st.ReconAccess("fresh", "ROOM"); v { t.Fatal("default ROOM should be denied") }

	// global on -> fresh user inherits
	if err := st.SetReconType("ROOM", true); err != nil { t.Fatal(err) }
	if v, _ := st.ReconAccess("fresh", "ROOM"); !v { t.Fatal("fresh should inherit global ROOM") }

	// per-user deny override beats global
	if err := st.SetReconOverride("fresh", "ROOM", false); err != nil { t.Fatal(err) }
	if v, _ := st.ReconAccess("fresh", "ROOM"); v { t.Fatal("per-user deny should beat global ROOM") }

	// clear overrides -> back to global
	if _, err := st.ClearReconOverrides("fresh"); err != nil { t.Fatal(err) }
	if v, _ := st.ReconAccess("fresh", "ROOM"); !v { t.Fatal("after clear, fresh should inherit global ROOM again") }

	// per-user grant beats global off
	if err := st.SetReconType("TEACHER", false); err != nil { t.Fatal(err) }
	if err := st.SetReconOverride("fresh", "TEACHER", true); err != nil { t.Fatal(err) }
	if v, _ := st.ReconAccess("fresh", "TEACHER"); !v { t.Fatal("per-user grant should beat global off") }

	// RevokeAll -> everything off
	if _, err := st.RevokeAll(); err != nil { t.Fatal(err) }
	for _, ty := range []string{"TEACHER", "ROOM", "SUBJECT"} {
		if v, _ := st.ReconAccess("fresh", ty); v { t.Fatalf("after RevokeAll, %s should be denied", ty) }
	}
}
