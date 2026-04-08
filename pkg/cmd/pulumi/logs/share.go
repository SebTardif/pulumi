// Copyright 2026, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logs

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/nacl/box"

	"github.com/pulumi/pulumi/pkg/v3/backend/display"
	cmdBackend "github.com/pulumi/pulumi/pkg/v3/cmd/pulumi/backend"
	"github.com/pulumi/pulumi/pkg/v3/cmd/pulumi/constrictor"
	cmdStack "github.com/pulumi/pulumi/pkg/v3/cmd/pulumi/stack"
	"github.com/pulumi/pulumi/pkg/v3/engine/encryptedlog"
	pkgWorkspace "github.com/pulumi/pulumi/pkg/v3/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/cmdutil"
)

func newShareCmd(ws pkgWorkspace.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "share <filename>",
		Short: "Re-encrypt a log file for sharing with Pulumi support",
		Long: "Create a copy of a log file that can be safely shared with\n" +
			"Pulumi support. The log content is re-encrypted with a key\n" +
			"that only Pulumi can read.\n" +
			"\n" +
			"For encrypted (PLOG) files, the session key is re-encrypted\n" +
			"with Pulumi's public key and the body is copied as-is.\n" +
			"\n" +
			"For gzip-compressed files, the content is encrypted with\n" +
			"a fresh session key protected by Pulumi's public key.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			stackName, _ := cmd.Flags().GetString("stack")
			filename := args[0]

			pulumiKey, keyID, err := fetchPulumiPublicKey(ctx)
			if err != nil {
				return err
			}
			sealEnc := &sealedBoxEncrypter{publicKey: pulumiKey, keyID: keyID}

			outPath := strings.TrimSuffix(filename, filepath.Ext(filename)) + ".shared.log"

			f, err := os.Open(filename)
			if err != nil {
				return fmt.Errorf("opening log file: %w", err)
			}
			defer f.Close()

			var magic [4]byte
			if _, err := io.ReadFull(f, magic[:]); err != nil {
				return fmt.Errorf("reading log file: %w", err)
			}
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("seeking log file: %w", err)
			}

			if string(magic[:]) == encryptedlog.Magic {
				err = sharePLOG(ctx, ws, stackName, f, outPath, sealEnc)
			} else {
				err = shareGzip(ctx, f, outPath, sealEnc)
			}
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "Shared log written to %s\n\n", outPath)
			fmt.Fprintf(os.Stderr, "You can safely attach this file to a GitHub issue or\n")
			fmt.Fprintf(os.Stderr, "send it to Pulumi support. Only Pulumi can decrypt it.\n")
			return nil
		},
	}

	constrictor.AttachArguments(cmd, &constrictor.Arguments{
		Arguments: []constrictor.Argument{{Name: "filename"}},
		Required:  1,
	})

	return cmd
}

// sharePLOG re-encrypts a PLOG file's session key with Pulumi's public
// key and copies the body chunks verbatim. Only the header changes.
func sharePLOG(
	ctx context.Context, ws pkgWorkspace.Context, stackName string,
	f *os.File, outPath string, sealEnc *sealedBoxEncrypter,
) error {
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return fmt.Errorf("reading magic: %w", err)
	}
	var version [1]byte
	if _, err := io.ReadFull(f, version[:]); err != nil {
		return fmt.Errorf("reading version: %w", err)
	}
	var keyLenBuf [2]byte
	if _, err := io.ReadFull(f, keyLenBuf[:]); err != nil {
		return fmt.Errorf("reading key length: %w", err)
	}
	keyLen := binary.BigEndian.Uint16(keyLenBuf[:])
	encryptedKey := make([]byte, keyLen)
	if _, err := io.ReadFull(f, encryptedKey); err != nil {
		return fmt.Errorf("reading encrypted key: %w", err)
	}

	// Decrypt the session key using the stack's secrets manager.
	if stackName == "" {
		stackName = stackNameFromFilename(filepath.Base(f.Name()))
	}
	if stackName == "" {
		return fmt.Errorf("cannot determine stack from filename %q; use --stack to specify", filepath.Base(f.Name()))
	}

	opts := display.Options{Color: cmdutil.GetGlobalColorization()}
	s, err := cmdStack.RequireStack(
		ctx, cmdutil.Diag(), ws,
		cmdBackend.DefaultLoginManager,
		stackName, cmdStack.LoadOnly, opts,
	)
	if err != nil {
		return fmt.Errorf("loading stack %q: %w", stackName, err)
	}

	sm, err := secretsManagerFromStack(ctx, s)
	if err != nil {
		return fmt.Errorf("getting secrets manager for stack %q: %w", stackName, err)
	}

	sessionKeyPlain, err := sm.Decrypter().DecryptValue(ctx, string(encryptedKey))
	if err != nil {
		return fmt.Errorf("decrypting session key: %w", err)
	}

	// Re-encrypt the session key with Pulumi's public key.
	newEncryptedKey, err := sealEnc.EncryptValue(ctx, sessionKeyPlain)
	if err != nil {
		return fmt.Errorf("re-encrypting session key: %w", err)
	}

	outFile, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	defer outFile.Close()

	if err := encryptedlog.WriteHeader(outFile, []byte(newEncryptedKey)); err != nil {
		os.Remove(outPath)
		return fmt.Errorf("writing header: %w", err)
	}

	if _, err := io.Copy(outFile, f); err != nil {
		os.Remove(outPath)
		return fmt.Errorf("copying body: %w", err)
	}

	return nil
}

// shareGzip encrypts a gzip-compressed log file with a fresh session
// key protected by Pulumi's public key.
func shareGzip(
	ctx context.Context, f *os.File, outPath string, sealEnc *sealedBoxEncrypter,
) error {
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("log file is neither encrypted nor gzip-compressed: %w", err)
	}
	defer gz.Close()

	plaintext, err := io.ReadAll(gz)
	if err != nil {
		return fmt.Errorf("decompressing log: %w", err)
	}

	outFile, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	defer outFile.Close()

	w, err := encryptedlog.NewWriter(ctx, outFile, sealEnc)
	if err != nil {
		os.Remove(outPath)
		return fmt.Errorf("creating encrypted writer: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		os.Remove(outPath)
		return fmt.Errorf("writing shared log: %w", err)
	}
	if err := w.Close(); err != nil {
		os.Remove(outPath)
		return fmt.Errorf("closing shared log: %w", err)
	}

	return nil
}

// sealedBoxEncrypter encrypts the session key using X25519 sealed box
// (NaCl anonymous box) with Pulumi's public key. The key ID is embedded
// in the ciphertext so Pulumi knows which private key to use.
type sealedBoxEncrypter struct {
	publicKey [32]byte
	keyID     string
}

func (e *sealedBoxEncrypter) EncryptValue(_ context.Context, plaintext string) (string, error) {
	sealed, err := box.SealAnonymous(nil, []byte(plaintext), &e.publicKey, rand.Reader)
	if err != nil {
		return "", fmt.Errorf("sealed box encryption failed: %w", err)
	}
	return fmt.Sprintf("sealed-box:%s:%s", e.keyID, base64.StdEncoding.EncodeToString(sealed)), nil
}

func (e *sealedBoxEncrypter) BatchEncrypt(ctx context.Context, secrets []string) ([]string, error) {
	return config.DefaultBatchEncrypt(ctx, e, secrets)
}

// fetchPulumiPublicKey retrieves a fresh public key from the Pulumi API.
// It is a variable so tests can replace it with a mock key.
var fetchPulumiPublicKey = fetchPulumiPublicKeyFromAPI

func fetchPulumiPublicKeyFromAPI(_ context.Context) (publicKey [32]byte, keyID string, err error) {
	// TODO: Call Pulumi API: POST /api/log-sharing/keys
	// Response: { "keyId": "...", "publicKey": "base64..." }
	return [32]byte{}, "", fmt.Errorf("Pulumi log-sharing API is not yet available")
}
