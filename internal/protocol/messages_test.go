package protocol

import (
	"bytes"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	validNames := []string{
		"test.txt",
		"报告-2026.pdf",
		"a",
		"hello_world.tar.gz",
	}
	for _, n := range validNames {
		if err := ValidateName(n); err != nil {
			t.Errorf("expected valid name %q, got error: %v", n, err)
		}
	}

	invalidNames := []string{
		"",                               // empty
		".",                              // dot
		"..",                             // dot dot
		"folder/file.txt",                // forward slash
		"folder\\file.txt",               // backslash
		"file.txt ",                      // trailing space
		"file.txt.",                      // trailing dot
		"bad\x00name",                    // null char
		"control\nname",                  // control char
		strings.Repeat("a", 256),         // over 255 bytes
		string([]byte{0xff, 0xfe, 0xfd}), // invalid utf-8
	}
	for _, n := range invalidNames {
		if err := ValidateName(n); err == nil {
			t.Errorf("expected error for invalid name %q, got nil", n)
		}
	}
}

func TestHelloValidation(t *testing.T) {
	validHello := Hello{
		ClientMaxStreams: 8,
		ClientMaxChunk:   65536,
		ClientWindow:     1048576,
	}
	if err := ValidateHello(validHello); err != nil {
		t.Fatalf("expected valid hello, got: %v", err)
	}

	badStreams := validHello
	badStreams.ClientMaxStreams = 0
	if err := ValidateHello(badStreams); err == nil {
		t.Error("expected error for ClientMaxStreams = 0")
	}

	badChunk := validHello
	badChunk.ClientMaxChunk = 1024
	if err := ValidateHello(badChunk); err == nil {
		t.Error("expected error for ClientMaxChunk < 4096")
	}
}

func TestMarshalUnmarshalRoundtrip(t *testing.T) {
	// 1. PutRequest
	req := PutRequest{
		RequestID:  RequestID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		FileName:   "test_file.dat",
		FileSize:   1024 * 1024,
		FileSHA256: Digest{0xaa, 0xbb, 0xcc},
	}
	p, err := Marshal(req)
	if err != nil {
		t.Fatalf("Marshal PutRequest failed: %v", err)
	}
	var outReq PutRequest
	if err := Unmarshal(p, &outReq); err != nil {
		t.Fatalf("Unmarshal PutRequest failed: %v", err)
	}
	if outReq != req {
		t.Errorf("PutRequest mismatch: expected %+v, got %+v", req, outReq)
	}

	// 2. PutResponse
	resp := PutResponse{
		TransferID: TransferID{9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 1, 2, 3, 4, 5, 6},
		State:      ResponseActive,
		NextOffset: 65536,
	}
	p, err = Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal PutResponse failed: %v", err)
	}
	var outResp PutResponse
	if err := Unmarshal(p, &outResp); err != nil {
		t.Fatalf("Unmarshal PutResponse failed: %v", err)
	}
	if outResp != resp {
		t.Errorf("PutResponse mismatch: expected %+v, got %+v", resp, outResp)
	}

	// 3. Data
	data := Data{
		FileOffset: 123456,
		Bytes:      []byte("hello secure transfer protocol!"),
	}
	p, err = Marshal(data)
	if err != nil {
		t.Fatalf("Marshal Data failed: %v", err)
	}
	var outData Data
	if err := Unmarshal(p, &outData); err != nil {
		t.Fatalf("Unmarshal Data failed: %v", err)
	}
	if outData.FileOffset != data.FileOffset || !bytes.Equal(outData.Bytes, data.Bytes) {
		t.Errorf("Data mismatch: expected %+v, got %+v", data, outData)
	}

	// 4. RemoteError
	rErr := RemoteError{
		Code:    CodeUnauthorized,
		Message: "证书未通过认证",
	}
	p, err = Marshal(rErr)
	if err != nil {
		t.Fatalf("Marshal RemoteError failed: %v", err)
	}
	var outErr RemoteError
	if err := Unmarshal(p, &outErr); err != nil {
		t.Fatalf("Unmarshal RemoteError failed: %v", err)
	}
	if outErr.Code != rErr.Code || outErr.Message != rErr.Message {
		t.Errorf("RemoteError mismatch: expected %+v, got %+v", rErr, outErr)
	}
}

func TestListRoundtrip(t *testing.T) {
	listReq := ListRequest{After: "a.txt"}
	p, err := Marshal(listReq)
	if err != nil {
		t.Fatalf("Marshal ListRequest failed: %v", err)
	}
	var outListReq ListRequest
	if err := Unmarshal(p, &outListReq); err != nil {
		t.Fatalf("Unmarshal ListRequest failed: %v", err)
	}
	if outListReq.After != listReq.After {
		t.Errorf("ListRequest mismatch: expected %q, got %q", listReq.After, outListReq.After)
	}

	listResp := ListResponse{
		More: true,
		Files: []FileEntry{
			{FileName: "a.txt", Size: 100, SHA256: Digest{1}},
			{FileName: "b.txt", Size: 200, SHA256: Digest{2}},
			{FileName: "c.txt", Size: 300, SHA256: Digest{3}},
		},
	}
	p, err = Marshal(listResp)
	if err != nil {
		t.Fatalf("Marshal ListResponse failed: %v", err)
	}
	var outListResp ListResponse
	if err := Unmarshal(p, &outListResp); err != nil {
		t.Fatalf("Unmarshal ListResponse failed: %v", err)
	}
	if outListResp.More != listResp.More || len(outListResp.Files) != 3 {
		t.Fatalf("ListResponse metadata mismatch: %+v", outListResp)
	}
	for i := range listResp.Files {
		if outListResp.Files[i] != listResp.Files[i] {
			t.Errorf("entry %d mismatch: expected %+v, got %+v", i, listResp.Files[i], outListResp.Files[i])
		}
	}
}
