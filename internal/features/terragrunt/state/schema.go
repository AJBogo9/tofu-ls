// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"io"
	"log"

	"github.com/hashicorp/go-memdb"
	globalState "github.com/opentofu/tofu-ls/internal/state"
)

const (
	terragruntTableName = "terragrunt"
)

var dbSchema = &memdb.DBSchema{
	Tables: map[string]*memdb.TableSchema{
		terragruntTableName: {
			Name: terragruntTableName,
			Indexes: map[string]*memdb.IndexSchema{
				"id": {
					Name:    "id",
					Unique:  true,
					Indexer: &memdb.StringFieldIndex{Field: "path"},
				},
			},
		},
	},
}

func NewTerragruntStore(changeStore *globalState.ChangeStore) (*TerragruntStore, error) {
	db, err := memdb.NewMemDB(dbSchema)
	if err != nil {
		return nil, err
	}
	discardLogger := log.New(io.Discard, "", 0)

	return &TerragruntStore{
		db:          db,
		tableName:   terragruntTableName,
		logger:      discardLogger,
		changeStore: changeStore,
	}, nil
}
