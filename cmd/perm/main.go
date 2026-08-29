package main

import (
	"flag"
	"fmt"
	"log"

	"untis-proxy/internal/store"
)

func main() {
	db := flag.String("db", "untis.db", "sqlite database path")
	username := flag.String("user", "", "username to grant/revoke access for")
	all := flag.Bool("all", false, "apply to every user (revoke only)")
	grant := flag.Bool("grant", false, "grant the feature")
	revoke := flag.Bool("revoke", false, "revoke the feature")
	flag.Parse()

	st, err := store.Open(*db)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	if *all {
		if *grant {
			log.Fatal("--all can only be used with --revoke")
		}
		n, err := st.RevokeAll()
		if err != nil {
			log.Fatalf("revoke all: %v", err)
		}
		fmt.Printf("revoked recon access for %d user(s)\n", n)
		return
	}
	if *username == "" {
		log.Fatal("--user is required (or use --all --revoke)")
	}
	if *grant == *revoke {
		log.Fatal("exactly one of --grant or --revoke is required")
	}

	allowed := *grant
	if err := st.SetFeature(*username, "recon", allowed); err != nil {
		log.Fatalf("set feature: %v", err)
	}
	action := "revoked"
	if allowed {
		action = "granted"
	}
	on, _ := st.FeatureEnabled(*username, "recon")
	fmt.Printf("recon access for %q %s (now enabled=%v)\n", *username, action, on)
}
