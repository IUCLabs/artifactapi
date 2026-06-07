package auth

import (
	"encoding/base64"
	"net/http"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func BasicHeaders(remote models.Remote) http.Header {
	h := http.Header{}
	if remote.Username != "" {
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString(
			[]byte(remote.Username+":"+remote.Password),
		))
	}
	return h
}
