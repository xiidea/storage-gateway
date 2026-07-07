package backend

import (
	"strings"
	"testing"
)

func TestNormalizeGCSCredentials(t *testing.T) {
	key := `{"type":"service_account","project_id":"p","client_email":"x@p.iam.gserviceaccount.com","private_key":"---"}`

	cases := []struct {
		name    string
		config  string
		want    string
		wantErr string
	}{
		{
			name:   "canonical wrapped object",
			config: `{"credentials_json":` + key + `}`,
			want:   key,
		},
		{
			name:   "stringified key",
			config: `{"credentials_json":"{\"type\":\"service_account\",\"project_id\":\"p\"}"}`,
			want:   `{"type":"service_account","project_id":"p"}`,
		},
		{
			name:   "bare service account key as whole config",
			config: key,
			want:   key,
		},
		{
			name:    "empty config",
			config:  `{}`,
			wantErr: "credentials_json",
		},
		{
			name:    "null credentials",
			config:  `{"credentials_json":null}`,
			wantErr: "credentials_json",
		},
		{
			name:    "non-service-account bare object",
			config:  `{"type":"authorized_user"}`,
			wantErr: "credentials_json",
		},
		{
			name:    "malformed json",
			config:  `{`,
			wantErr: "parsing gcs config",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeGCSCredentials([]byte(c.config))
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}
