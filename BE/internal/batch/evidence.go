package batch

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type Manifest struct {
	ExecutionID                  string
	DatabaseID, Version, ActorID int64
	SQL                          string
	TableOID                     uint32
	ScopeCount                   int64
	ScopeHash                    string
}

type Row struct {
	Key           int64
	Before, After json.RawMessage
}

type Checkpoint struct {
	BatchNo, LastKey, RowCount, CommittedRows int64
	RowHash, PreviousSignature, Signature     string
	AttemptExecutionID                        string `json:",omitempty"`
	AttemptVersion                            int64  `json:",omitempty"`
}

func ManifestSignature(key []byte, m Manifest) string {
	data, _ := json.Marshal([]any{"terserah-batch-manifest-v1", m})
	return signature(key, data)
}

func RowsHash(rows []Row) (string, error) {
	data, err := json.Marshal(rows)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func CheckpointSignature(key []byte, manifestSignature string, c Checkpoint) string {
	c.Signature = ""
	data, _ := json.Marshal([]any{"terserah-batch-checkpoint-v1", manifestSignature, c})
	return signature(key, data)
}

func signature(key, data []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}
