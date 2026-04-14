// Package hostsfile manages a delimited block in /etc/hosts.
//
// Install and Uninstall are idempotent. They only touch lines between the
// BEGIN/END markers; anything outside is preserved byte-for-byte.
package hostsfile

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strings"
)

const (
	DefaultPath = "/etc/hosts"
	BeginMarker = "# BEGIN kubetunnel — managed block, do not edit"
	EndMarker   = "# END kubetunnel"
)

// Install ensures the given hostnames are mapped to 127.0.0.1 inside the
// managed block. If the block doesn't exist, it is appended. If it exists,
// it is rewritten in place.
func Install(path string, hostnames []string) error {
	if path == "" {
		path = DefaultPath
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read hosts: %w", err)
	}
	backup := path + ".kubetunnel.bak"
	if _, err := os.Stat(backup); os.IsNotExist(err) {
		if err := os.WriteFile(backup, existing, 0o644); err != nil {
			return fmt.Errorf("write backup: %w", err)
		}
	}

	newBlock := buildBlock(hostnames)
	out := replaceOrAppendBlock(existing, newBlock)
	if bytes.Equal(existing, out) {
		return nil
	}
	return atomicWrite(path, out, 0o644)
}

// Uninstall removes the managed block if present. Idempotent.
func Uninstall(path string) error {
	if path == "" {
		path = DefaultPath
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read hosts: %w", err)
	}
	out := removeBlock(existing)
	if bytes.Equal(existing, out) {
		return nil
	}
	return atomicWrite(path, out, 0o644)
}

// Contains returns true if the managed block is present.
func Contains(path string) (bool, error) {
	if path == "" {
		path = DefaultPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return hasBlock(data), nil
}

func buildBlock(hostnames []string) []byte {
	var b bytes.Buffer
	b.WriteString(BeginMarker)
	b.WriteByte('\n')
	for _, h := range hostnames {
		fmt.Fprintf(&b, "127.0.0.1\t%s\n", h)
	}
	b.WriteString(EndMarker)
	b.WriteByte('\n')
	return b.Bytes()
}

func hasBlock(data []byte) bool {
	return bytes.Contains(data, []byte(BeginMarker)) && bytes.Contains(data, []byte(EndMarker))
}

func replaceOrAppendBlock(data, block []byte) []byte {
	if !hasBlock(data) {
		if len(data) > 0 && data[len(data)-1] != '\n' {
			data = append(data, '\n')
		}
		return append(data, block...)
	}
	var out bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	inBlock := false
	written := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == BeginMarker {
			inBlock = true
			out.Write(block)
			written = true
			continue
		}
		if inBlock {
			if strings.TrimSpace(line) == EndMarker {
				inBlock = false
			}
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if !written {
		out.Write(block)
	}
	return out.Bytes()
}

func removeBlock(data []byte) []byte {
	if !hasBlock(data) {
		return data
	}
	var out bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	inBlock := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == BeginMarker {
			inBlock = true
			continue
		}
		if inBlock {
			if strings.TrimSpace(line) == EndMarker {
				inBlock = false
			}
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return bytes.TrimRight(out.Bytes(), "\n")
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir, base := dirAndBase(path)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func dirAndBase(p string) (string, string) {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i], p[i+1:]
		}
	}
	return ".", p
}
