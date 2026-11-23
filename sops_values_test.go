package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pastedEncValue = "ENC[AES256_GCM,data:lcZN/V/vQ+aB78miNlPDad2Z,iv:t91vtivgg2LwCQP+9alg13Dc9FCB5GsOdBa6+vlqF34=,tag:YD9xUCdh1YB8NQu4oyOM1A==,type:str]"

func TestSopsEncryptedValues(t *testing.T) {
	values := sopsEncryptedValues([]byte(`# a comment may mention ENC[AES256_GCM,data:x] freely
software:
  - source:
      path: ./packages
  - source:
      github:
        repo: thin-edge/tedge-container-bundle
        token: ` + pastedEncValue + `
configuration:
  - name: mosquitto
    content: ` + pastedEncValue + `
sops:
  mac: ` + pastedEncValue + `
`))
	assert.Equal(t, []string{
		"software[1].source.github.token (line 8)",
		"configuration[0].content (line 11)",
	}, values, "the sops section itself is not reported")
	assert.Empty(t, sopsEncryptedValues([]byte("tenantOptions:\n  - value: ENC is fine without the bracket form\n")))
}

// TestLoadManifestRejectsEncValuesWithoutSopsMetadata: ENC[...] values
// copied into a file without a sops section would otherwise be sent as
// literal text (e.g. as a GitHub token, failing with 401 Bad credentials)
func TestLoadManifestRejectsEncValuesWithoutSopsMetadata(t *testing.T) {
	path := writeManifest(t, "software:\n  - source:\n      github:\n        repo: thin-edge/tedge-container-bundle\n        token: "+pastedEncValue+"\n")
	_, err := LoadManifest(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path+" contains sops encrypted values but no sops metadata, so they can't be decrypted: software[0].source.github.token (line 5)")
	assert.Contains(t, err.Error(), "tokenFrom: {env: NAME}")
}

func TestLoadManifestRejectsEncValuesInIncludedFile(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"base.yaml":   "tenantOptions:\n  - category: a\n    key: b\n    value: " + pastedEncValue + "\n",
		"tenant.yaml": "include: [base.yaml]\n",
	})
	_, err := LoadManifest(filepath.Join(dir, "tenant.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "base.yaml contains sops encrypted values but no sops metadata")
	assert.Contains(t, err.Error(), "tenantOptions[0].value (line 4)")
}

func TestLoadManifestRejectsValuesEncryptedTwice(t *testing.T) {
	// The decrypted manifest still has an ENC[...] value
	fakeSops(t, "cat <<'EOF'\nusers:\n  - email: a@example.invalid\n    password: "+pastedEncValue+"\nEOF")
	path := writeManifest(t, encryptedManifest)
	_, err := LoadManifest(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has values which are still encrypted after decrypting it, i.e. encrypted twice")
	assert.Contains(t, err.Error(), "users[0].password (line 3)")
}

func TestSecretsFileRejectsEncValuesWithoutSopsMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenant.secrets.yaml")
	require.NoError(t, os.WriteFile(path, []byte("secrets:\n  admin:\n    value: "+pastedEncValue+"\n"), 0o600))
	_, err := OpenSecretStore(path, SecretsEncryptionAuto)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "secrets.admin.value (line 3)")
}
