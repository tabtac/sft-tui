package protocol

import (
	"bytes"
	"strings"
	"testing"
)

func TestRemoteErrorString(t *testing.T) {
	r := &RemoteError{Code: CodeUnauthorized, Message: "未授权"}
	if !strings.Contains(r.Error(), "0x0010") || !strings.Contains(r.Error(), "未授权") {
		t.Errorf("unexpected Error() output: %q", r.Error())
	}
}

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

func TestMarshalUnmarshalAllTypes(t *testing.T) {
	// 1. HelloAck
	ha := HelloAck{
		MaxStreams:     8,
		ChunkSize:      65536,
		WindowSize:     1048576,
		IdleTimeoutSec: 120,
	}
	p, err := Marshal(ha)
	if err != nil {
		t.Fatalf("Marshal HelloAck failed: %v", err)
	}
	var outHA HelloAck
	if err := Unmarshal(p, &outHA); err != nil || outHA != ha {
		t.Fatalf("HelloAck mismatch: %+v vs %+v, err=%v", ha, outHA, err)
	}

	// 2. PutRequest & PutResponse
	putReq := PutRequest{
		RequestID:  RequestID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		FileName:   "test_file.dat",
		FileSize:   1024 * 1024,
		FileSHA256: Digest{0xaa, 0xbb, 0xcc},
	}
	p, err = Marshal(putReq)
	if err != nil {
		t.Fatalf("Marshal PutRequest failed: %v", err)
	}
	var outPutReq PutRequest
	if err := Unmarshal(p, &outPutReq); err != nil || outPutReq != putReq {
		t.Fatalf("PutRequest mismatch: %+v vs %+v, err=%v", putReq, outPutReq, err)
	}

	putResp := PutResponse{
		TransferID: TransferID{9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 1, 2, 3, 4, 5, 6},
		State:      ResponseActive,
		NextOffset: 65536,
	}
	p, err = Marshal(putResp)
	if err != nil {
		t.Fatalf("Marshal PutResponse failed: %v", err)
	}
	var outPutResp PutResponse
	if err := Unmarshal(p, &outPutResp); err != nil || outPutResp != putResp {
		t.Fatalf("PutResponse mismatch: %+v vs %+v, err=%v", putResp, outPutResp, err)
	}

	// 3. GetRequest & GetResponse
	getReq := GetRequest{
		RequestID: RequestID{1, 1, 1},
		FileName:  "get.txt",
	}
	p, err = Marshal(getReq)
	if err != nil {
		t.Fatalf("Marshal GetRequest failed: %v", err)
	}
	var outGetReq GetRequest
	if err := Unmarshal(p, &outGetReq); err != nil || outGetReq != getReq {
		t.Fatalf("GetRequest mismatch: %+v vs %+v, err=%v", getReq, outGetReq, err)
	}

	getResp := GetResponse{
		TransferID:  TransferID{2, 2, 2},
		State:       ResponseActive,
		FileSize:    2048,
		FileSHA256:  Digest{0x12, 0x34},
		StartOffset: 0,
	}
	p, err = Marshal(getResp)
	if err != nil {
		t.Fatalf("Marshal GetResponse failed: %v", err)
	}
	var outGetResp GetResponse
	if err := Unmarshal(p, &outGetResp); err != nil || outGetResp != getResp {
		t.Fatalf("GetResponse mismatch: %+v vs %+v, err=%v", getResp, outGetResp, err)
	}

	// 4. Data & Ack & Commit & Finish
	data := Data{
		FileOffset: 123456,
		Bytes:      []byte("hello secure transfer protocol!"),
	}
	p, err = Marshal(data)
	if err != nil {
		t.Fatalf("Marshal Data failed: %v", err)
	}
	var outData Data
	if err := Unmarshal(p, &outData); err != nil || outData.FileOffset != data.FileOffset || !bytes.Equal(outData.Bytes, data.Bytes) {
		t.Fatalf("Data mismatch: %+v vs %+v, err=%v", data, outData, err)
	}

	ack := Ack{NextOffset: 65536}
	p, _ = Marshal(ack)
	var outAck Ack
	_ = Unmarshal(p, &outAck)
	if outAck != ack {
		t.Errorf("Ack mismatch")
	}

	commit := Commit{FileSize: 100, FileSHA256: Digest{5}}
	p, _ = Marshal(commit)
	var outCommit Commit
	_ = Unmarshal(p, &outCommit)
	if outCommit != commit {
		t.Errorf("Commit mismatch")
	}

	fin := Finish{TransferID: TransferID{7}, FileSize: 100, FileSHA256: Digest{5}}
	p, _ = Marshal(fin)
	var outFin Finish
	_ = Unmarshal(p, &outFin)
	if outFin != fin {
		t.Errorf("Finish mismatch")
	}

	// 5. ResumeRequest & ResumeResponse
	resReq := ResumeRequest{
		TransferID:   TransferID{3, 3, 3},
		Direction:    DirectionUpload,
		ClientOffset: 0,
		FileSHA256:   Digest{0x55},
	}
	p, _ = Marshal(resReq)
	var outResReq ResumeRequest
	_ = Unmarshal(p, &outResReq)
	if outResReq != resReq {
		t.Errorf("ResumeRequest mismatch")
	}

	resResp := ResumeResponse{
		State:          ResponseActive,
		Direction:      DirectionUpload,
		FileSize:       1000,
		FileSHA256:     Digest{0x55},
		AcceptedOffset: 500,
	}
	p, _ = Marshal(resResp)
	var outResResp ResumeResponse
	_ = Unmarshal(p, &outResResp)
	if outResResp != resResp {
		t.Errorf("ResumeResponse mismatch")
	}

	// 6. Cancel & CancelAck & GoAway
	cancel := Cancel{Mode: CancelPause}
	p, _ = Marshal(cancel)
	var outCancel Cancel
	_ = Unmarshal(p, &outCancel)
	if outCancel != cancel {
		t.Errorf("Cancel mismatch")
	}

	cancelAck := CancelAck{Mode: CancelPause, AcceptedOffset: 123}
	p, _ = Marshal(cancelAck)
	var outCancelAck CancelAck
	_ = Unmarshal(p, &outCancelAck)
	if outCancelAck != cancelAck {
		t.Errorf("CancelAck mismatch")
	}

	goAway := GoAway{ReasonCode: CodeBusy, LastAcceptedStreamID: 3, RetryAfterSec: 10}
	p, _ = Marshal(goAway)
	var outGoAway GoAway
	_ = Unmarshal(p, &outGoAway)
	if outGoAway != goAway {
		t.Errorf("GoAway mismatch")
	}

	// 7. RemoteError (值与指针序列化)
	rErr := RemoteError{
		Code:    CodeUnauthorized,
		Message: "证书未通过认证",
	}
	p, err = Marshal(rErr)
	if err != nil {
		t.Fatalf("Marshal RemoteError failed: %v", err)
	}
	pPtr, err := Marshal(&rErr)
	if err != nil || !bytes.Equal(p, pPtr) {
		t.Fatalf("Marshal *RemoteError failed or mismatched: err=%v", err)
	}
	var outErr RemoteError
	if err := Unmarshal(p, &outErr); err != nil || outErr.Code != rErr.Code || outErr.Message != rErr.Message {
		t.Fatalf("RemoteError mismatch: %+v vs %+v, err=%v", rErr, outErr, err)
	}

	// 8. DATA 超过 MaxChunkSize 应拒绝反序列化
	oversizedData := make([]byte, DataOffsetSize+MaxChunkSize+1)
	if err := Unmarshal(oversizedData, &outData); err == nil {
		t.Error("expected error for oversized DATA payload in Unmarshal")
	}

	// 8. 反序列化时末尾包含多余字节应报错
	extraBytes := append(p, 0x00, 0x01)
	if err := Unmarshal(extraBytes, &outErr); err == nil {
		t.Error("expected error for trailing bytes in Unmarshal")
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
