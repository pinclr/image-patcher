/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import "testing"

func TestClassifyLogTail(t *testing.T) {
	// Inputs are abbreviated log tails representative of the real Kaniko /
	// apt / curl error blocks we want to distinguish. Each row should
	// exercise exactly one rule of the classifier; the "private registry"
	// row also verifies the documented Auth-wins-over-NotFound priority.
	cases := []struct {
		name string
		log  string
		want string
	}{
		{
			name: "kaniko manifest unknown",
			log:  `error building image: GET https://registry.example.com/v2/foo/manifests/nosuch: MANIFEST_UNKNOWN: manifest unknown`,
			want: FailureLabelBaseImageNotFound,
		},
		{
			name: "docker hub manifest not found",
			log:  `error: manifest for ubuntu:nosuchtag not found`,
			want: FailureLabelBaseImageNotFound,
		},
		{
			name: "registry 401 unauthorized",
			log:  `unauthorized: authentication required`,
			want: FailureLabelAuthorizationNeeded,
		},
		{
			name: "registry push denied",
			log:  `denied: requested access to the resource is denied`,
			want: FailureLabelAuthorizationNeeded,
		},
		{
			name: "no basic auth creds",
			log:  `no basic auth credentials`,
			want: FailureLabelAuthorizationNeeded,
		},
		{
			name: "private registry surfaces both auth and manifest, auth wins",
			log: `error checking push permissions -- HEAD https://private.io/v2/x: UNAUTHORIZED: authentication required
GET https://private.io/v2/x/manifests/latest: MANIFEST_UNKNOWN: manifest unknown`,
			want: FailureLabelAuthorizationNeeded,
		},
		{
			name: "apt mirror unreachable",
			log:  `E: Failed to fetch http://mirror.example.com/ubuntu/dists/jammy/InRelease  Temporary failure in name resolution`,
			want: FailureLabelNetworkError,
		},
		{
			name: "curl dns failure",
			log:  `curl: (6) Could not resolve host: files.internal`,
			want: FailureLabelNetworkError,
		},
		{
			name: "curl connection refused",
			log:  `curl: (7) Failed to connect to files.internal port 80: Connection refused`,
			want: FailureLabelNetworkError,
		},
		{
			name: "curl timeout",
			log:  `curl: (28) Operation timed out after 30000 milliseconds`,
			want: FailureLabelNetworkError,
		},
		{
			name: "i/o timeout in go http client",
			log:  `dial tcp 10.0.0.1:443: i/o timeout`,
			want: FailureLabelNetworkError,
		},
		{
			name: "oom killed -> unknown",
			log:  `command terminated with exit code 137`,
			want: FailureLabelControllerInternalError,
		},
		{
			name: "empty log -> unknown",
			log:  ``,
			want: FailureLabelControllerInternalError,
		},
		{
			name: "garbage log -> unknown",
			log:  `the quick brown fox`,
			want: FailureLabelControllerInternalError,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyLogTail(c.log)
			if got != c.want {
				t.Errorf("classifyLogTail(%q) = %q, want %q", c.log, got, c.want)
			}
		})
	}
}
