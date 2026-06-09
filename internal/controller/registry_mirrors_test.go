/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"os"
	"sort"
	"strings"
	"testing"

	omsv1alpha1 "image-patch-operator/api/v1alpha1"
)

func TestRegistryMirrors_EmitsRegistryMapArgs(t *testing.T) {
	mirrors := map[string]string{
		"gcr.io":  "gcr.m.daocloud.io",
		"quay.io": "quay.m.daocloud.io",
	}
	args := kanikoArgsWithMirrors(omsv1alpha1.BuildOptions{}, "", "", mirrors)

	var got []string
	for _, a := range args {
		if strings.HasPrefix(a, "--registry-map=") {
			got = append(got, a)
		}
	}
	want := []string{
		"--registry-map=gcr.io=gcr.m.daocloud.io",
		"--registry-map=quay.io=quay.m.daocloud.io",
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("registry-map args mismatch\nwant: %v\ngot:  %v", want, got)
	}
}

func TestRegistryMirrors_DockerIONormalizedToIndexDockerIO(t *testing.T) {
	args := kanikoArgsWithMirrors(omsv1alpha1.BuildOptions{}, "", "", map[string]string{
		"docker.io": "docker.m.daocloud.io",
	})
	var got string
	for _, a := range args {
		if strings.HasPrefix(a, "--registry-map=") {
			got = a
		}
	}
	want := "--registry-map=index.docker.io=docker.m.daocloud.io"
	if got != want {
		t.Errorf("docker.io should be rewritten to index.docker.io\nwant: %q\ngot:  %q", want, got)
	}
}

func TestRegistryMirrors_EmptyMapOmitsFlag(t *testing.T) {
	args := kanikoArgsWithMirrors(omsv1alpha1.BuildOptions{}, "", "", nil)
	for _, a := range args {
		if strings.HasPrefix(a, "--registry-map") {
			t.Errorf("empty mirrors: %q should be omitted; got args: %v", a, args)
		}
	}
}

func TestRegistryMirrorsFromEnv(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{"empty", "", nil},
		{"single", "docker.io=docker.m.daocloud.io", map[string]string{"docker.io": "docker.m.daocloud.io"}},
		{"multi", "docker.io=docker.m.daocloud.io,gcr.io=gcr.m.daocloud.io", map[string]string{
			"docker.io": "docker.m.daocloud.io",
			"gcr.io":    "gcr.m.daocloud.io",
		}},
		{"whitespace", " docker.io = docker.m.daocloud.io , gcr.io=gcr.m.daocloud.io ", map[string]string{
			"docker.io": "docker.m.daocloud.io",
			"gcr.io":    "gcr.m.daocloud.io",
		}},
		{"skip-malformed", "docker.io=docker.m.daocloud.io,bogus,=mirror,gcr.io=", map[string]string{
			"docker.io": "docker.m.daocloud.io",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("KANIKO_REGISTRY_MIRRORS", c.raw)
			got := RegistryMirrorsFromEnv()
			if len(got) != len(c.want) {
				t.Fatalf("len mismatch: want %v, got %v", c.want, got)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Errorf("key %q: want %q, got %q", k, v, got[k])
				}
			}
		})
	}
	_ = os.Unsetenv("KANIKO_REGISTRY_MIRRORS")
}
