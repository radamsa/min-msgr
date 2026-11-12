// Command min-msgr-devkey creates a local user, chat, and API key for manual
// testing before the OAuth web flow exists. It is not part of the server.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/dav/min-msgr/internal/config"
	"github.com/dav/min-msgr/internal/db"
	"github.com/dav/min-msgr/internal/service"
)

func main() {
	login := flag.String("login", "", "OAuth login for the new user")
	label := flag.String("label", "", "chat and API key label")
	email := flag.String("email", "", "optional email")
	flag.Parse()

	if *login == "" || *label == "" {
		flag.Usage()
		os.Exit(2)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(1)
	}

	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		fmt.Fprintln(os.Stderr, "migrate database:", err)
		os.Exit(1)
	}

	user, err := service.NewUserService(pool).Create(ctx, *login, *email)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create user:", err)
		os.Exit(1)
	}
	key, err := service.NewKeyService(pool).Create(ctx, user.ID, *label)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create key:", err)
		os.Exit(1)
	}

	fmt.Printf("user: %s\nchat: %d\nkey: %s\n", user.PublicID, key.ChatID, key.PlainKey)
}
