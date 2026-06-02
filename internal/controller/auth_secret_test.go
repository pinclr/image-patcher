package controller

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// authsOf unmarshals a config.json blob and returns its auths map keyed by
// registry, with each value rendered back to a string for easy comparison.
func authsOf(t *testing.T, cfg []byte) map[string]string {
	t.Helper()
	var doc struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	if err := json.Unmarshal(cfg, &doc); err != nil {
		t.Fatalf("unmarshal merged config: %v", err)
	}
	out := map[string]string{}
	for reg, v := range doc.Auths {
		out[reg] = string(v)
	}
	return out
}

func TestMergeDockerConfigs_OverlayWinsAndUnions(t *testing.T) {
	base := []byte(`{"auths":{"push.example.com":{"auth":"PUSH"},"shared.example.com":{"auth":"BASE"}},"credHelpers":{"gcr.io":"gcloud"}}`)
	overlay := []byte(`{"auths":{"pull.example.com":{"auth":"PULL"},"shared.example.com":{"auth":"OVERLAY"}}}`)

	merged, err := mergeDockerConfigs(base, overlay)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	auths := authsOf(t, merged)
	if got := auths["push.example.com"]; got != `{"auth":"PUSH"}` {
		t.Errorf("push auth not preserved: %s", got)
	}
	if got := auths["pull.example.com"]; got != `{"auth":"PULL"}` {
		t.Errorf("pull auth not added: %s", got)
	}
	if got := auths["shared.example.com"]; got != `{"auth":"OVERLAY"}` {
		t.Errorf("overlay should win on conflict, got: %s", got)
	}

	// Non-auths top-level keys from base survive the merge.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}
	if _, ok := doc["credHelpers"]; !ok {
		t.Error("credHelpers from base dropped")
	}
}

func TestMergeDockerConfigs_EmptyInputs(t *testing.T) {
	// Empty overlay leaves base auths intact.
	merged, err := mergeDockerConfigs([]byte(`{"auths":{"a":{"auth":"A"}}}`), nil)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := authsOf(t, merged)["a"]; got != `{"auth":"A"}` {
		t.Errorf("base auth lost with empty overlay: %s", got)
	}

	// Both empty yields a valid config with an empty auths map.
	merged, err = mergeDockerConfigs(nil, nil)
	if err != nil {
		t.Fatalf("merge empty: %v", err)
	}
	if len(authsOf(t, merged)) != 0 {
		t.Errorf("expected empty auths, got %s", merged)
	}
}

func TestExtractDockerConfig_KeyVariants(t *testing.T) {
	generic := &corev1.Secret{Data: map[string][]byte{"config.json": []byte(`{"auths":{}}`)}}
	if got := extractDockerConfig(generic); string(got) != `{"auths":{}}` {
		t.Errorf("config.json key not read: %s", got)
	}

	dockercfg := &corev1.Secret{Data: map[string][]byte{".dockerconfigjson": []byte(`{"auths":{"x":{}}}`)}}
	if got := extractDockerConfig(dockercfg); string(got) != `{"auths":{"x":{}}}` {
		t.Errorf(".dockerconfigjson key not read: %s", got)
	}

	if got := extractDockerConfig(&corev1.Secret{}); got != nil {
		t.Errorf("expected nil for secret with neither key, got: %s", got)
	}
}
