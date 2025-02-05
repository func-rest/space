package models

import (
	"github.com/func-rest/space/tools/types"
)

var _ Model = (*Param)(nil)

const (
	ParamAppSettings = "settings"
)

type Param struct {
	BaseModel

	Key   string        `db:"key" json:"key"`
	Value types.JsonRaw `db:"value" json:"value"`
}

func (m *Param) TableName() string {
	return "_params"
}
