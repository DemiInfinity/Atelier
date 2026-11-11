package workflow

import "strings"

// DeployPagesAction is the built-in action that uploads a Pages site
// (`uses: atelier/deploy-pages@v1` with `path: dist`). The runner implements it (internal/runner/pages_deploy.go), so it's never
// downloaded or pinned to a commit.
const DeployPagesAction = "atelier/deploy-pages"

// IsBuiltin reports whether the reference is an action built into the runner.
func (r ActionRef) IsBuiltin() bool {
	return r.Kind == UsesRemote && r.Path == "" && strings.EqualFold(r.Owner+"/"+r.Repo, DeployPagesAction)
}
