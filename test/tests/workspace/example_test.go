// Copyright (c) 2020 Gitpod GmbH. All rights reserved.
// Licensed under the GNU Affero General Public License (AGPL).
// See License-AGPL.txt in the project root for license information.

package workspace_test

import (
	"github.com/gitpod-io/gitpod/test/pkg/integration"
	agent "github.com/gitpod-io/gitpod/test/tests/workspace/workspace_agent/api"
	"testing"
)

func TestWorkspaceInstrumentation(t *testing.T) {
	it := integration.NewTest(t)
	defer it.Done()

	nfo := integration.LaunchWorkspaceFromContextURL(it, "github.com/gitpod-io/gitpod")
	defer integration.DeleteWorkspace(it, nfo.LatestInstance.ID)

	rsa, err := it.Instrument(integration.ComponentWorkspace, "workspace", integration.WithInstanceID(nfo.LatestInstance.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer rsa.Close()

	var ls agent.ListDirResponse
	err = rsa.Call("WorkspaceAgent.ListDir", &agent.ListDirRequest{
		Dir: "/workspace/gitpod",
	}, &ls)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range ls.Files {
		t.Log(f)
	}
}

func TestLaunchWorkspaceDirectly(t *testing.T) {
	it := integration.NewTest(t)
	defer it.Done()

	nfo := integration.LaunchWorkspaceDirectly(it)
	defer integration.DeleteWorkspace(it, nfo.Req.Id)
}
