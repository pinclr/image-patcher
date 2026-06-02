/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	omsv1alpha1 "image-patch-operator/api/v1alpha1"
)

// kanikoArgs returns the args slice the generated Kaniko container
// would be invoked with, for assertion convenience.
func kanikoArgs(buildOpts omsv1alpha1.BuildOptions, buildCacheRepo, dedupRef string) []string {
	cr := &omsv1alpha1.ImagePatch{}
	cr.Name = "tc"
	cr.Namespace = "ns"
	j := constructJob(cr, "tc-job", "tc-cm", "ns", "registry.local/app:v1", "kaniko:test",
		buildCacheRepo, "tc-docker-auth",
		corev1.ResourceRequirements{}, buildOpts, dedupRef)
	return j.Spec.Template.Spec.Containers[0].Args
}

func TestDisableBuildLayerCache_OmitsCacheFlags(t *testing.T) {
	on := true
	withFlag := kanikoArgs(omsv1alpha1.BuildOptions{DisableBuildLayerCache: &on}, "registry.local/cache", "")
	withoutFlag := kanikoArgs(omsv1alpha1.BuildOptions{}, "registry.local/cache", "")

	for _, a := range withFlag {
		if a == "--cache=true" || strings.HasPrefix(a, "--cache-repo=") {
			t.Errorf("DisableBuildLayerCache=true: %q should be omitted; got args: %v", a, withFlag)
		}
	}
	if !hasArg(withoutFlag, "--cache=true") || !hasArgPrefix(withoutFlag, "--cache-repo=") {
		t.Errorf("DisableBuildLayerCache unset: --cache=true / --cache-repo= should be present; got args: %v", withoutFlag)
	}
}

func TestDisableBuildCache_DropsDedupDestination(t *testing.T) {
	// constructJob doesn't itself read DisableBuildCache -- the gate
	// lives in Reconcile, which is responsible for passing "" as
	// dedupDestination when the flag is set. This test pins the
	// constructJob side: empty dedupRef means no second --destination,
	// matching the contract Reconcile relies on.
	args := kanikoArgs(omsv1alpha1.BuildOptions{}, "", "")
	destCount := 0
	for _, a := range args {
		if strings.HasPrefix(a, "--destination=") {
			destCount++
		}
	}
	if destCount != 1 {
		t.Errorf("dedupRef empty: expected 1 --destination, got %d in %v", destCount, args)
	}
}

func TestMergeBuildOptions_DisableFlagOverride(t *testing.T) {
	off := false
	on := true

	// Chart default off, CR-set on: CR wins.
	got := mergeBuildOptions(
		omsv1alpha1.BuildOptions{DisableBuildCache: &off},
		&omsv1alpha1.BuildOptions{DisableBuildCache: &on},
	)
	if !boolPtrTrue(got.DisableBuildCache) {
		t.Errorf("CR-set DisableBuildCache=true should override default false; got %v", got.DisableBuildCache)
	}

	// Chart default on, CR nil: default wins.
	got = mergeBuildOptions(
		omsv1alpha1.BuildOptions{DisableBuildCache: &on},
		&omsv1alpha1.BuildOptions{},
	)
	if !boolPtrTrue(got.DisableBuildCache) {
		t.Errorf("CR-unset should inherit chart default true; got %v", got.DisableBuildCache)
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func hasArgPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}
