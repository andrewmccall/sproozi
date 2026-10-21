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

package envtest

import "context"

// Shared constants used across envtest test files to satisfy goconst lint rules.
const (
	testRuntimeName         = "codex"
	testPolicyName          = "sre"
	testEgressModule        = "go-modules"
	testSproozDemoNamespace = "sproozi-demo"
	testPodsResource        = "pods"
)

const testMCPCapability = "mcp.docs"

// testGatewayResolver stands in for the controller's DNS lookup of the gateway
// Service, which does not exist in envtest.
func testGatewayResolver(context.Context, string) (string, error) { return "10.96.0.42", nil }
