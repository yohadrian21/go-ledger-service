// Command apikey generates an API key for the ledger service.
//
// It prints two lines on stdout: the key, which must be handed to the client
// over a secure channel, and the AUTH_API_KEYS entry for the service, which
// contains only the key's SHA-256 digest. The key itself is not stored
// anywhere.
//
//	go run ./cmd/apikey -client acme -role client
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/yohadrian21/go-ledger-service/internal/auth"
	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

func main() {
	client := flag.String("client", "", "client ID the key belongs to (required)")
	role := flag.String("role", string(usecase.RoleClient), "role granted to the key: client or operator")
	flag.Parse()

	if err := run(*client, usecase.Role(*role)); err != nil {
		fmt.Fprintln(os.Stderr, "apikey:", err)
		os.Exit(2)
	}
}

func run(client string, role usecase.Role) error {
	key, err := auth.NewKey()
	if err != nil {
		return err
	}
	entry, err := auth.Entry(client, role, key)
	if err != nil {
		return err
	}
	fmt.Println(key)
	fmt.Println(entry)
	return nil
}
