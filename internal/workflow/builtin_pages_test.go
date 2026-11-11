package workflow

import "testing"

func TestDeployPagesIsBuiltin(t *testing.T) {
	r, err := ParseUses("atelier/deploy-pages@v1")
	if err != nil || !r.IsBuiltin() || r.NeedsPin() {
		t.Fatalf("%+v %v builtin=%v needsPin=%v", r, err, r.IsBuiltin(), r.NeedsPin())
	}
	for _, u := range []string{"atelier:atelier/deploy-pages@v1", "atelier/deploy-pages/sub@v1", "atelier/other@v1"} {
		if r, _ := ParseUses(u); r.IsBuiltin() || !r.NeedsPin() {
			t.Errorf("%s treated as built in", u)
		}
	}
}
