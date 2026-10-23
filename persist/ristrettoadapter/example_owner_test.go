package ristrettoadapter_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/dgraph-io/ristretto/v2"
	"github.com/gtkit/gincache/persist/ristrettoadapter"
	"time"
)

func ExampleNewOwner() {
	value, err := exampleOwner()
	if err != nil {
		return
	}
	if _, err := fmt.Println(value); err != nil {
		return
	}
	// Output: business http
}

func exampleOwner() (result string, err error) {
	owner, err := ristrettoadapter.NewOwner(ristretto.Config[string, []byte]{MaxCost: 16 << 20, NumCounters: 10000, BufferItems: 64})
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, owner.Close(context.Background())) }()
	business, err := owner.NewStore("business.v1")
	if err != nil {
		return "", err
	}
	response, err := owner.NewStore("http.v1")
	if err != nil {
		return "", err
	}
	if err := business.Set("same-key", "business", time.Minute); err != nil {
		return "", err
	}
	if err := response.Set("same-key", "http", time.Minute); err != nil {
		return "", err
	}
	var a, b string
	if err := business.Get("same-key", &a); err != nil {
		return "", err
	}
	if err := response.Get("same-key", &b); err != nil {
		return "", err
	}
	return a + " " + b, nil
}
