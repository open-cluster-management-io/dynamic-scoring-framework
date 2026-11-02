/*
Copyright 2025.

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

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestE2E runs the chart installation and managed-cluster add-on e2e suite in a temporary Kind cluster.
func TestE2E(t *testing.T) {
	if os.Getenv("DSF_E2E") != "true" {
		t.Skip("e2e tests require the isolated environment created by make test-e2e")
	}
	kindCluster := os.Getenv("KIND_CLUSTER")
	if kindCluster == "" || os.Getenv("KUBECONFIG") == "" {
		t.Fatal("make test-e2e must set KIND_CLUSTER and KUBECONFIG")
	}
	output, err := exec.Command("kubectl", "config", "current-context").CombinedOutput()
	if err != nil {
		t.Fatalf("read current kube context: %v\n%s", err, output)
	}
	if got, want := strings.TrimSpace(string(output)), fmt.Sprintf("kind-%s", kindCluster); got != want {
		t.Fatalf("e2e context = %q; want isolated Kind context %q", got, want)
	}
	RegisterFailHandler(Fail)
	RunSpecs(t, "e2e suite")
}
