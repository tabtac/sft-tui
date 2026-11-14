package protocol

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
)

type ListRequest struct {
	After string // 分页游标，按文件名升序排列
}

type FileEntry struct {
	FileName string
	Size     uint64
	SHA256   Digest
}

type ListResponse struct {
	More  bool
	Files []FileEntry
}

func badListError() error {
	return &RemoteError{Code: CodeBadFrame, Message: "文件列表字段、长度或排序非法"}
}

func marshalListRequest(x ListRequest) ([]byte, error) {
	if x.After != "" {
		if err := ValidateName(x.After); err != nil {
			return nil, err
		}
	}
	p := make([]byte, 2+len(x.After))
	binary.BigEndian.PutUint16(p, uint16(len(x.After)))
	copy(p[2:], x.After)
	return p, nil
}

func unmarshalListRequest(p []byte, x *ListRequest) error {
	if len(p) < 2 || int(binary.BigEndian.Uint16(p[:2])) != len(p)-2 || len(p)-2 > MaxFileNameBytes {
		return badListError()
	}
	x.After = string(p[2:])
	if x.After != "" {
		return ValidateName(x.After)
	}
	return nil
}

func marshalListResponse(x ListResponse) ([]byte, error) {
	if len(x.Files) > ListPageSize || (x.More && len(x.Files) == 0) {
		return nil, badListError()
	}
	var b bytes.Buffer
	if x.More {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	_ = binary.Write(&b, binary.BigEndian, uint16(len(x.Files)))

	previous := ""
	for _, f := range x.Files {
		if ValidateName(f.FileName) != nil || f.FileName <= previous || f.Size > math.MaxInt64 {
			return nil, badListError()
		}
		_ = binary.Write(&b, binary.BigEndian, uint16(len(f.FileName)))
		b.WriteString(f.FileName)
		_ = binary.Write(&b, binary.BigEndian, f.Size)
		b.Write(f.SHA256[:])
		previous = f.FileName
	}
	return b.Bytes(), nil
}

func unmarshalListResponse(p []byte, x *ListResponse) error {
	if len(p) < 3 || len(p) > MaxControlPayload || p[0] > 1 {
		return badListError()
	}
	count := int(binary.BigEndian.Uint16(p[1:3]))
	if count > ListPageSize || (p[0] == 1 && count == 0) {
		return badListError()
	}
	r := bytes.NewReader(p[3:])
	x.More = (p[0] == 1)
	x.Files = make([]FileEntry, 0, count)

	previous := ""
	for i := 0; i < count; i++ {
		var nameLen uint16
		if binary.Read(r, binary.BigEndian, &nameLen) != nil || nameLen == 0 || nameLen > MaxFileNameBytes || int(nameLen)+40 > r.Len() {
			return badListError()
		}
		nameBytes := make([]byte, nameLen)
		_, _ = io.ReadFull(r, nameBytes)
		entry := FileEntry{FileName: string(nameBytes)}
		if ValidateName(entry.FileName) != nil || entry.FileName <= previous {
			return badListError()
		}
		if binary.Read(r, binary.BigEndian, &entry.Size) != nil || entry.Size > math.MaxInt64 {
			return badListError()
		}
		if _, err := io.ReadFull(r, entry.SHA256[:]); err != nil {
			return badListError()
		}
		x.Files = append(x.Files, entry)
		previous = entry.FileName
	}

	if r.Len() != 0 {
		return badListError()
	}
	return nil
}
