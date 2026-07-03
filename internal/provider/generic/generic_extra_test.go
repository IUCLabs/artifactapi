package generic

import (
	"testing"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestGenericRewriteResponse(t *testing.T) {
	if out, err := (&Provider{}).RewriteResponse([]byte("x"), models.Remote{}, "http://p"); out != nil || err != nil {
		t.Error("generic never rewrites")
	}
}
