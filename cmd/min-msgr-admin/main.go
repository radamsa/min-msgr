// Command min-msgr-admin manages admins, blocks, overrides, and global
// settings directly in the database. It is not part of the server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/dav/min-msgr/internal/config"
	"github.com/dav/min-msgr/internal/db"
	"github.com/dav/min-msgr/internal/domain"
	"github.com/dav/min-msgr/internal/service"
)

func main() {
	ctx := context.Background()

	flag.Usage = func() { usage() }
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(1)
	}
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

	admins := service.NewAdminService(pool)
	users := service.NewUserService(pool)

	var code int
	switch flag.Arg(0) {
	case "promote":
		code = cmdSetAdmin(ctx, admins, users, true)
	case "demote":
		code = cmdSetAdmin(ctx, admins, users, false)
	case "block", "unblock":
		code = cmdSetBlocked(ctx, admins, users, flag.Arg(0) == "block")
	case "delete-user":
		code = cmdDeleteUser(ctx, admins, users)
	case "set-limits":
		code = cmdSetLimits(ctx, admins)
	case "set-override":
		code = cmdSetOverride(ctx, admins, users)
	case "clear-override":
		code = cmdClearOverride(ctx, admins, users)
	case "show":
		code = cmdShow(ctx, admins)
	default:
		usage()
		code = 2
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: min-msgr-admin <command> [args]

commands:
  promote <login|#id>          grant the admin role
  demote <login|#id>           revoke the admin role
  block <login|#id>            block a user (kills sessions, rejects keys)
  unblock <login|#id>          unblock a user
  delete-user <login|#id>      delete a user with all chats, keys, messages
  set-limits <keys> <days> <bytes-per-chat> <total-bytes>
                               update global admin defaults
  set-override <user> [keys] [days] [bytes-per-chat] [total-bytes]
                               store per-user overrides ("-" keeps default)
  clear-override <user>        drop all overrides for a user
  show                         print global settings and users

user argument: either the OAuth login or the numeric ID (e.g. "alice#123").
`)
}

func cmdSetAdmin(ctx context.Context, admins *service.AdminService, users *service.UserService, admin bool) int {
	user, ok := resolveUser(ctx, users, flag.Args())
	if !ok {
		return 2
	}
	if err := admins.SetAdmin(ctx, user.ID, admin); err != nil {
		return fail("set admin role", err)
	}
	fmt.Printf("%s: admin = %v\n", user.PublicID, admin)
	return 0
}

func cmdSetBlocked(ctx context.Context, admins *service.AdminService, users *service.UserService, blocked bool) int {
	user, ok := resolveUser(ctx, users, flag.Args())
	if !ok {
		return 2
	}
	if err := admins.SetBlocked(ctx, user.ID, blocked); err != nil {
		return fail("set block status", err)
	}
	fmt.Printf("%s: blocked = %v\n", user.PublicID, blocked)
	return 0
}

func cmdDeleteUser(ctx context.Context, admins *service.AdminService, users *service.UserService) int {
	user, ok := resolveUser(ctx, users, flag.Args())
	if !ok {
		return 2
	}
	if err := admins.DeleteUser(ctx, user.ID); err != nil {
		return fail("delete user", err)
	}
	fmt.Printf("%s: deleted\n", user.PublicID)
	return 0
}

func cmdSetLimits(ctx context.Context, admins *service.AdminService) int {
	args := flag.Args()[1:]
	if len(args) != 4 {
		usage()
		return 2
	}
	numbers, ok := parsePositive(args)
	if !ok {
		return fail("parse limits", errors.New("all four limits must be positive integers"))
	}
	view, err := admins.UpdateSettings(ctx, int(numbers[0]), int(numbers[1]), numbers[2], numbers[3])
	if err != nil {
		return fail("update settings", err)
	}
	fmt.Printf("settings: keys=%d days=%d bytes_per_chat=%d total_bytes=%d\n",
		view.MaxKeysPerUser, view.MaxDaysLimit, view.MaxBytesPerChat, view.MaxTotalBytesPerUser)
	return 0
}

func cmdSetOverride(ctx context.Context, admins *service.AdminService, users *service.UserService) int {
	args := flag.Args()[1:]
	if len(args) < 1 || len(args) > 5 {
		usage()
		return 2
	}
	user, ok := resolveUser(ctx, users, []string{"set-override", args[0]})
	if !ok {
		return 2
	}
	var ints []*int
	var int64s []*int64
	for _, value := range append(args[1:], make([]string, 5-len(args[1:]))...) {
		if value == "" || value == "-" {
			ints = append(ints, nil)
			int64s = append(int64s, nil)
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n <= 0 {
			return fail("parse override", fmt.Errorf("%q must be a positive integer or \"-\"", value))
		}
		i := int(n)
		ints = append(ints, &i)
		int64s = append(int64s, &n)
	}
	if err := admins.SetOverride(ctx, user.ID, ints[0], ints[1], int64s[2], int64s[3]); err != nil {
		return fail("set override", err)
	}
	fmt.Printf("%s: override saved\n", user.PublicID)
	return 0
}

func cmdClearOverride(ctx context.Context, admins *service.AdminService, users *service.UserService) int {
	user, ok := resolveUser(ctx, users, flag.Args())
	if !ok {
		return 2
	}
	if err := admins.DeleteOverride(ctx, user.ID); err != nil {
		return fail("clear override", err)
	}
	fmt.Printf("%s: override cleared\n", user.PublicID)
	return 0
}

func cmdShow(ctx context.Context, admins *service.AdminService) int {
	settings, err := admins.Settings(ctx)
	if err != nil {
		return fail("read settings", err)
	}
	fmt.Printf("settings: keys=%d days=%d bytes_per_chat=%d total_bytes=%d\n",
		settings.MaxKeysPerUser, settings.MaxDaysLimit,
		settings.MaxBytesPerChat, settings.MaxTotalBytesPerUser)

	users, err := admins.Users(ctx)
	if err != nil {
		return fail("list users", err)
	}
	for _, u := range users {
		flags := ""
		if u.IsAdmin {
			flags += " admin"
		}
		if u.IsBlocked {
			flags += " blocked"
		}
		fmt.Printf("user %s chats=%d%s\n", u.PublicID, u.ChatCount, flags)
	}
	return 0
}

// resolveUser finds a user by "<login>#NNN" public ID, bare numeric ID, or
// OAuth login (unique match required).
func resolveUser(ctx context.Context, users *service.UserService, args []string) (service.User, bool) {
	if len(args) < 2 {
		usage()
		return service.User{}, false
	}
	key := strings.TrimSpace(args[1])

	find := func(err error) (service.User, bool) {
		fail("find user", err)
		return service.User{}, false
	}

	if strings.Contains(key, "#") {
		user, err := users.ByPublicID(ctx, key)
		if err != nil {
			return find(err)
		}
		return user, true
	}
	if id, err := strconv.ParseInt(key, 10, 64); err == nil {
		user, err := users.ByID(ctx, id)
		if err != nil {
			return find(err)
		}
		return user, true
	}

	usersByLogin, err := users.ByLogin(ctx, key)
	if err != nil {
		return find(err)
	}
	if len(usersByLogin) == 0 {
		return find(domain.ErrNotFound)
	}
	if len(usersByLogin) > 1 {
		var ids []string
		for _, u := range usersByLogin {
			ids = append(ids, u.PublicID)
		}
		return find(fmt.Errorf("login %q is ambiguous: %s", key, strings.Join(ids, ", ")))
	}
	return usersByLogin[0], true
}

func parsePositive(args []string) ([]int64, bool) {
	values := make([]int64, 0, len(args))
	for _, arg := range args {
		n, err := strconv.ParseInt(arg, 10, 64)
		if err != nil || n <= 0 {
			return nil, false
		}
		values = append(values, n)
	}
	return values, true
}

func fail(action string, err error) int {
	if errors.Is(err, domain.ErrNotFound) {
		fmt.Fprintf(os.Stderr, "%s: not found\n", action)
		return 1
	}
	fmt.Fprintf(os.Stderr, "%s: %v\n", action, err)
	return 1
}
