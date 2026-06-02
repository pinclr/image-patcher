/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package registry

import (
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

func TestKeychainFromDockerConfig_ResolvesKnownRegistry(t *testing.T) {
	// auth "dXNlcjpwYXNz" decodes to "user:pass".
	cfg := []byte(`{"auths":{"registry.example.com":{"auth":"dXNlcjpwYXNz"}}}`)
	kc, err := keychainFromDockerConfig(cfg)
	if err != nil {
		t.Fatalf("keychainFromDockerConfig: %v", err)
	}

	reg, err := name.NewRegistry("registry.example.com")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	a, err := kc.Resolve(reg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	authCfg, err := a.Authorization()
	if err != nil {
		t.Fatalf("Authorization: %v", err)
	}
	if authCfg.Auth != "dXNlcjpwYXNz" {
		t.Errorf("expected auth carried through, got %+v", authCfg)
	}
}

func TestKeychainFromDockerConfig_UnknownRegistryIsAnonymous(t *testing.T) {
	kc, err := keychainFromDockerConfig([]byte(`{"auths":{"registry.example.com":{"auth":"dXNlcjpwYXNz"}}}`))
	if err != nil {
		t.Fatalf("keychainFromDockerConfig: %v", err)
	}
	other, err := name.NewRegistry("other.example.com")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	a, err := kc.Resolve(other)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if a != authn.Anonymous {
		t.Errorf("unknown registry should resolve to Anonymous, got %#v", a)
	}
}

func TestKeychainFromDockerConfig_InvalidJSON(t *testing.T) {
	if _, err := keychainFromDockerConfig([]byte(`{not json`)); err == nil {
		t.Error("expected parse error for malformed docker config")
	}
}
