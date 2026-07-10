package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseCredentialInput(t *testing.T) {
	atJWT := "eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjE3MDAwMDAwMDB9.sig"
	stVal := "eyJhbGciOiJkaXIiLCJlbmMiOiJBMjU2R0NNIn0..abc.def.ghi"

	t.Run("access only", func(t *testing.T) {
		got, ok := parseCredentialInput(atJWT)
		if !ok || got.AccessToken != atJWT || got.SessionToken != "" {
			t.Fatalf("got %+v ok=%v", got, ok)
		}
	})

	t.Run("session only", func(t *testing.T) {
		got, ok := parseCredentialInput("st:" + stVal)
		if !ok || got.SessionToken != stVal || got.AccessToken != "" {
			t.Fatalf("got %+v ok=%v", got, ok)
		}
	})

	t.Run("access----session", func(t *testing.T) {
		got, ok := parseCredentialInput(atJWT + "----" + stVal)
		if !ok || got.AccessToken != atJWT || got.SessionToken != stVal {
			t.Fatalf("got %+v ok=%v", got, ok)
		}
	})

	t.Run("session json", func(t *testing.T) {
		raw := `{"accessToken":"` + atJWT + `","sessionToken":"mysessiontokenvalue1234567890abcdefghij"}`
		got, ok := parseCredentialInput(raw)
		if !ok || got.AccessToken != atJWT {
			t.Fatalf("got %+v", got)
		}
		if got.SessionToken != "mysessiontokenvalue1234567890abcdefghij" {
			t.Fatalf("st=%q", got.SessionToken)
		}
	})
}

func TestNormalizeSessionToken(t *testing.T) {
	got := normalizeSessionToken("__Secure-next-auth.session-token=secretvalue")
	if got != "secretvalue" {
		t.Fatalf("got %q", got)
	}
}

func TestIsAccessTokenRejectsJWE(t *testing.T) {
	jwe := "eyJhbGciOiJkaXIiLCJlbmMiOiJBMjU2R0NNIn0..abc.def.ghi"
	if isAccessToken(jwe) {
		t.Fatal("JWE session token must not be treated as access token")
	}
	at := "eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjE3MDAwMDAwMDB9.sig"
	if !isAccessToken(at) {
		t.Fatal("JWT access token should be accepted")
	}
}

func TestSplitUploadTextMultilineJSON(t *testing.T) {
	raw := "{\n\"accessToken\":\"eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjE3MDAwMDAwMDB9.sig\",\n\"sessionToken\":\"mysessiontokenvalue1234567890abcdefghij\"\n}\n"
	chunks := splitUploadText(raw)
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	got, ok := parseCredentialInput(chunks[0])
	if !ok || got.AccessToken == "" || got.SessionToken == "" {
		t.Fatalf("parse failed: %+v ok=%v", got, ok)
	}
}

func TestCleanTokenInvalidFallsBack(t *testing.T) {
	if got := cleanToken("ee74bb8ce3invalid"); got != "" {
		t.Fatalf("invalid token should become empty, got %q", got)
	}
	at := "eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjE3MDAwMDAwMDB9.sig"
	if got := cleanToken(at + "----" + "eyJhbGciOiJkaXIiLCJlbmMiOiJBMjU2R0NNIn0..x.y.z"); got != at {
		t.Fatalf("cleanToken ---- format: got %q", got)
	}
}

func TestSaveLoadTokenJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	at := "eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjE3MDAwMDAwMDB9.sig"
	st := "eyJhbGciOiJkaXIiLCJlbmMiOiJBMjU2R0NNIn0..abc.def.ghi"
	tok := newStoredToken(at, st, "")
	if err := saveTokensToFile(path, []storedToken{tok}); err != nil {
		t.Fatal(err)
	}
	loaded := loadTokensFromFile(path)
	if len(loaded) != 1 {
		t.Fatalf("want 1, got %d", len(loaded))
	}
	if loaded[0].AccessToken != at || loaded[0].SessionToken != st {
		t.Fatalf("loaded %+v", loaded[0])
	}
	var tf tokenFile
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &tf); err != nil || tf.Version != tokenFileVersion {
		t.Fatalf("file format: %v version=%d", err, tf.Version)
	}
}

func TestParseJWTExp(t *testing.T) {
	exp := parseJWTExp("eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJleHAiOjQxMDI0NDQ4MDB9.x")
	if exp.Unix() != 4102444800 {
		t.Fatalf("exp unix=%d", exp.Unix())
	}
}
