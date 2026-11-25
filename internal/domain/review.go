package domain

import "encoding/json"

// ImportProposal 是持久预检结果；Preview 的 created 表示候选新增，尚未写入账本。
type ImportProposal struct {
	ID        string        `json:"id"`
	Request   ImportRequest `json:"request"`
	Preview   ImportResult  `json:"preview"`
	Committed bool          `json:"committed"`
}

// ImportReview 投影已有来源行及处理状态，不重复存储原图或来源正文。
type ImportReview struct {
	ID            string          `json:"id"`
	Kind          string          `json:"kind"`
	State         string          `json:"state"`
	Source        string          `json:"source"`
	Record        ImportRecord    `json:"record"`
	TransactionID *string         `json:"transaction_id"`
	Resolution    json.RawMessage `json:"resolution"`
}

// ReviewDecision 必须显式指定动作和原因；Record 用于修正异常行或更正已有关联交易。
type ReviewDecision struct {
	Action        string        `json:"action"`
	Reason        string        `json:"reason"`
	TransactionID string        `json:"transaction_id,omitempty"`
	Record        *ImportRecord `json:"record,omitempty"`
}

// SourceRange 仅表示已入库记录的观察范围；没有账单完整覆盖证据时 ConfirmedCoverage 永远为空。
type SourceRange struct {
	Source            string   `json:"source"`
	Account           string   `json:"account"`
	Count             int      `json:"count"`
	First             string   `json:"first"`
	Last              string   `json:"last"`
	ConfirmedCoverage []string `json:"confirmed_coverage"`
}
