package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"untis-proxy/internal/store"
)

// untisctl is a small operator tool for managing a running untis-proxy's
// database: users, secrets, perms, the class pool and calendar tokens. It edits
// the SQLite DB directly, like cmd/perm and cmd/seed, so it can be run against
// a live server's data file.

func main() {
	db := flag.String("db", "untis.db", "path to the untis sqlite database")
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, `untisctl – manage an untis-proxy database

Usage:
  untisctl [-db PATH] <command> [options]

Commands:
  perms list [--user U]              show global switches and per-user overrides
  perms grant|revoke [--user U|--global] TYPE
                                     grant/revoke teacher|room|subject
  perms clear --user U               drop a user's overrides (fall back to global)
  perms reset                        wipe all permissions (class-pool-only default)

  users list                         list accounts
  users add --user U --secret S [--method password|key]
  users remove --user U              delete user + secret + overrides + personal tokens

  pool list                          show pooled classes and their owners
  pool owners                        show which user(s) own each class

  tokens list                        list calendar subscription tokens
  tokens revoke TOKEN                revoke a calendar token

  status                             db stats: users, pool, perms, recon, tokens

Global:
  -db PATH   sqlite database path (default "untis.db")

Run 'untisctl <command> -h' for per-command flags.
`)
	}
	flag.Parse()

	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	st, err := store.Open(*db)
	if err != nil {
		fatal("open db: %v", err)
	}
	defer st.Close()

	switch flag.Arg(0) {
	case "perms":
		cmdPerms(st, flag.Args()[1:])
	case "users":
		cmdUsers(st, flag.Args()[1:])
	case "pool":
		cmdPool(st, flag.Args()[1:])
	case "tokens":
		cmdTokens(st, flag.Args()[1:])
	case "status":
		cmdStatus(st)
	default:
		fatal("unknown command %q\n\nrun 'untisctl -h' for usage", flag.Arg(0))
	}
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "untisctl: "+format+"\n", a...)
	os.Exit(1)
}

// ---------------------------------------------------------------------------
// perms
// ---------------------------------------------------------------------------

var reconTypes = map[string]string{
	"teacher":  "TEACHER",
	"teachers": "TEACHER",
	"room":     "ROOM",
	"rooms":    "ROOM",
	"subject":  "SUBJECT",
	"subjects": "SUBJECT",
}

func cmdPerms(st *store.Store, args []string) {
	if len(args) == 0 {
		fatal("perms requires a subcommand (list|grant|revoke|clear|reset)\n\nrun 'untisctl perms -h' for help")
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("perms list", flag.ExitOnError)
		user := fs.String("user", "", "filter to a single user")
		fs.Parse(args[1:])
		permsList(st, *user)
	case "grant", "revoke":
		permSet(st, args[0], args[1:])
	case "clear":
		permClear(st, args[1:])
	case "reset":
		n, err := st.RevokeAll()
		if err != nil {
			fatal("reset perms: %v", err)
		}
		fmt.Printf("reset %d permission rows: all users back to class-pool-only\n", n)
	default:
		fatal("unknown perms subcommand %q", args[0])
	}
}

func permsList(st *store.Store, user string) {
	rows, err := st.AllPerms()
	if err != nil {
		fatal("list perms: %v", err)
	}
	if len(rows) == 0 {
		fmt.Println("no permissions set (class-pool-only for everyone)")
		return
	}
	// Summarise: show global switches and per-user overrides, most relevant first.
	type summary struct {
		feature, scope string
		allowed        bool
	}
	var seen []summary
	matched := false
	for _, r := range rows {
		scope := r.Username
		if r.Username == "*" {
			scope = "(global)"
		}
		if user != "" && r.Username != "*" && !strings.EqualFold(r.Username, user) {
			continue
		}
		if r.Username != "*" {
			matched = true
		}
		seen = append(seen, summary{r.Feature, scope, r.Allowed})
	}
	if user != "" && !matched {
		fmt.Printf("no per-user overrides for %q (falls back to global switches)\n", user)
	}
	fmt.Printf("%-9s %-12s %s\n", "TYPE", "SCOPE", "ALLOWED")
	for _, s := range seen {
		fmt.Printf("%-9s %-12s %v\n", s.feature, s.scope, s.allowed)
	}
}

func permSet(st *store.Store, action string, args []string) {
	fs := flag.NewFlagSet("perms "+action, flag.ExitOnError)
	user := fs.String("user", "", "per-user override")
	global := fs.Bool("global", false, "global switch for all users")
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		fatal("perms %s requires a TYPE (e.g. 'perms %s --user U teacher')", action, action)
	}
	typ, ok := reconTypes[strings.ToLower(rest[0])]
	if !ok {
		fatal("unknown type %q (use teacher|room|subject)", rest[0])
	}
	if *user != "" && *global {
		fatal("pick --user (per-user) or --global (all users), not both")
	}
	if *user == "" && !*global {
		fatal("require --user U or --global")
	}
	allowed := action == "grant"
	var err error
	if *global {
		err = st.SetReconType(typ, allowed)
	} else {
		err = st.SetReconOverride(*user, typ, allowed)
	}
	if err != nil {
		fatal("%s %s: %v", action, typ, err)
	}
	target := "(global)"
	if *user != "" {
		target = *user
		on, _ := st.ReconAccess(*user, typ)
		fmt.Printf("%s %s for %q (effective=%v)\n", past(action), typ, *user, on)
		return
	}
	fmt.Printf("%s %s for %s\n", past(action), typ, target)
}

func past(action string) string {
	if action == "revoke" {
		return "revoked"
	}
	return "granted"
}

func permClear(st *store.Store, args []string) {
	fs := flag.NewFlagSet("perms clear", flag.ExitOnError)
	user := fs.String("user", "", "user whose overrides to clear")
	fs.Parse(args)
	if *user == "" {
		fatal("perms clear requires --user")
	}
	n, err := st.ClearReconOverrides(*user)
	if err != nil {
		fatal("clear overrides: %v", err)
	}
	fmt.Printf("cleared %d override(s) for %q (falls back to global switches)\n", n, *user)
}

// ---------------------------------------------------------------------------
// users
// ---------------------------------------------------------------------------

func cmdUsers(st *store.Store, args []string) {
	if len(args) == 0 {
		fatal("users requires a subcommand (list|add|remove)")
	}
	switch args[0] {
	case "list":
		usersList(st)
	case "add":
		userAdd(st, args[1:])
	case "remove":
		userRemove(st, args[1:])
	default:
		fatal("unknown users subcommand %q", args[0])
	}
}

func usersList(st *store.Store) {
	users, err := st.ListUsers()
	if err != nil {
		fatal("list users: %v", err)
	}
	if len(users) == 0 {
		fmt.Println("no users")
		return
	}
	fmt.Printf("%-12s %-8s %-6s %-8s %-8s %-22s\n", "USER", "METHOD", "TYPE", "CLASS", "PERSON", "DISPLAY")
	for _, u := range users {
		fmt.Printf("%-12s %-8s %-6d %-8d %-8d %-22s\n", u.Username, u.Method, u.PersonType, u.ClassID, u.PersonID, u.DisplayName)
	}
}

func userAdd(st *store.Store, args []string) {
	fs := flag.NewFlagSet("users add", flag.ExitOnError)
	user := fs.String("user", "", "username")
	secret := fs.String("secret", "", "password or base32 TOTP key")
	method := fs.String("method", "key", "password or key")
	fs.Parse(args)
	if *user == "" || *secret == "" {
		fatal("users add requires --user and --secret")
	}
	if *method != "password" && *method != "key" {
		fatal("--method must be 'password' or 'key'")
	}
	u := &store.User{Username: *user, Password: *secret, Method: *method}
	if err := st.UpsertUser(u); err != nil {
		fatal("add user: %v", err)
	}
	if *method == "key" {
		if err := st.UpsertSecret(*user, *secret); err != nil {
			fatal("add secret: %v", err)
		}
	}
	fmt.Printf("added user %q (method=%s)\n", *user, *method)
}

func userRemove(st *store.Store, args []string) {
	fs := flag.NewFlagSet("users remove", flag.ExitOnError)
	user := fs.String("user", "", "username")
	fs.Parse(args)
	if *user == "" {
		fatal("users remove requires --user")
	}
	n, err := st.DeleteUser(*user)
	if err != nil {
		fatal("remove user: %v", err)
	}
	fmt.Printf("removed user %q (%d related row(s) cleaned)\n", *user, n)
}

// ---------------------------------------------------------------------------
// pool
// ---------------------------------------------------------------------------

func cmdPool(st *store.Store, args []string) {
	if len(args) == 0 {
		poolList(st)
		return
	}
	switch args[0] {
	case "list":
		poolList(st)
	case "owners":
		// list already shows owners; no distinct owners view needed
		poolList(st)
	default:
		fatal("unknown pool subcommand %q", args[0])
	}
}

func poolList(st *store.Store) {
	classes, err := st.Pool()
	if err != nil {
		fatal("pool list: %v", err)
	}
	if len(classes) == 0 {
		fmt.Println("no pooled classes (no users with a class_id)")
		return
	}
	fmt.Printf("%-8s %-20s %s\n", "CLASS", "NAME", "OWNER")
	for _, c := range classes {
		owner, _ := st.OwnerForClass(c.ID)
		ownerName := "-"
		if owner != nil {
			ownerName = owner.Username
		}
		fmt.Printf("%-8d %-20s %s\n", c.ID, c.Name, ownerName)
	}
}

// ---------------------------------------------------------------------------
// tokens
// ---------------------------------------------------------------------------

func cmdTokens(st *store.Store, args []string) {
	if len(args) == 0 {
		tokensList(st)
		return
	}
	switch args[0] {
	case "list":
		tokensList(st)
	case "revoke":
		if len(args) < 2 {
			fatal("tokens revoke requires a TOKEN")
		}
		n, err := st.DeleteClassToken(args[1])
		if err != nil {
			fatal("revoke token: %v", err)
		}
		if n == 0 {
			fmt.Println("token not found")
		} else {
			fmt.Println("token revoked")
		}
	default:
		fatal("unknown tokens subcommand %q", args[0])
	}
}

func tokensList(st *store.Store) {
	tokens, err := st.ListClassTokens()
	if err != nil {
		fatal("tokens list: %v", err)
	}
	if len(tokens) == 0 {
		fmt.Println("no calendar tokens")
		return
	}
	fmt.Printf("%-8s %-12s %-34s %-22s\n", "KIND", "SCHOOL", "TOKEN", "LAST ACCESS")
	for _, t := range tokens {
		kind := "class"
		target := fmt.Sprintf("%d", t.ClassID)
		if t.PersonID > 0 {
			kind = "person"
			target = fmt.Sprintf("p%d", t.PersonID)
		}
		last := "never"
		if t.LastAccess > 0 {
			last = time.Unix(t.LastAccess, 0).Format(time.RFC3339)
		}
		fmt.Printf("%-8s %-12s %-34s %-22s (%s)\n", kind, t.School, t.Token, last, target)
	}
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func cmdStatus(st *store.Store) {
	uc, _ := st.UserCount()
	pool, _ := st.Pool()
	rows, _ := st.AllPerms()
	tokens, _ := st.ListClassTokens()
	recon, _ := st.LoadReconElements()

	var permCount int
	var globalCount, overrideCount int
	for _, r := range rows {
		permCount++
		if r.Username == "*" {
			globalCount++
		} else {
			overrideCount++
		}
	}

	reconTotal := 0
	for _, ids := range recon {
		reconTotal += len(ids)
	}

	fmt.Printf("users:      %d\n", uc)
	fmt.Printf("pool:       %d classes\n", len(pool))
	fmt.Printf("recon:      %d elements (%d teachers, %d rooms, %d subjects)\n",
		reconTotal, len(recon["TEACHER"]), len(recon["ROOM"]), len(recon["SUBJECT"]))
	fmt.Printf("perms:      %d rows (%d global, %d per-user)\n", permCount, globalCount, overrideCount)
	fmt.Printf("tokens:     %d calendar tokens\n", len(tokens))
}
