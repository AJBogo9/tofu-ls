// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"log"

	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/hcl-lang/reference"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/features/terragrunt/ast"
	globalState "github.com/opentofu/tofu-ls/internal/state"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
	op "github.com/opentofu/tofu-ls/internal/tofu/module/operation"
)

type TerragruntStore struct {
	db        *memdb.MemDB
	tableName string
	logger    *log.Logger

	changeStore *globalState.ChangeStore
}

func (s *TerragruntStore) SetLogger(logger *log.Logger) {
	s.logger = logger
}

func (s *TerragruntStore) Add(path string) error {
	txn := s.db.Txn(true)
	defer txn.Abort()

	err := s.add(txn, path)
	if err != nil {
		return err
	}
	txn.Commit()

	return nil
}

func (s *TerragruntStore) add(txn *memdb.Txn, path string) error {
	obj, err := txn.First(s.tableName, "id", path)
	if err != nil {
		return err
	}
	if obj != nil {
		return &globalState.AlreadyExistsError{
			Idx: path,
		}
	}

	record := newTerragruntRecord(path)
	err = txn.Insert(s.tableName, record)
	if err != nil {
		return err
	}

	return s.queueRecordChange(nil, record)
}

func (s *TerragruntStore) AddIfNotExists(path string) error {
	txn := s.db.Txn(true)
	defer txn.Abort()

	_, err := terragruntRecordByPath(txn, path)
	if err != nil {
		if globalState.IsRecordNotFound(err) {
			err := s.add(txn, path)
			if err != nil {
				return err
			}
			txn.Commit()
			return nil
		}

		return err
	}

	return nil
}

func (s *TerragruntStore) Remove(path string) error {
	txn := s.db.Txn(true)
	defer txn.Abort()

	oldObj, err := txn.First(s.tableName, "id", path)
	if err != nil {
		return err
	}
	if oldObj == nil {
		// already removed
		return nil
	}

	err = s.queueRecordChange(oldObj.(*TerragruntRecord), nil)
	if err != nil {
		return err
	}

	_, err = txn.DeleteAll(s.tableName, "id", path)
	if err != nil {
		return err
	}

	txn.Commit()
	return nil
}

func (s *TerragruntStore) List() ([]*TerragruntRecord, error) {
	txn := s.db.Txn(false)

	it, err := txn.Get(s.tableName, "id")
	if err != nil {
		return nil, err
	}

	records := make([]*TerragruntRecord, 0)
	for item := it.Next(); item != nil; item = it.Next() {
		records = append(records, item.(*TerragruntRecord))
	}

	return records, nil
}

func (s *TerragruntStore) Exists(path string) bool {
	txn := s.db.Txn(false)

	obj, err := txn.First(s.tableName, "id", path)
	if err != nil {
		return false
	}

	return obj != nil
}

func (s *TerragruntStore) TerragruntRecordByPath(path string) (*TerragruntRecord, error) {
	txn := s.db.Txn(false)

	return terragruntRecordByPath(txn, path)
}

func terragruntRecordByPath(txn *memdb.Txn, path string) (*TerragruntRecord, error) {
	obj, err := txn.First(terragruntTableName, "id", path)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, &globalState.RecordNotFoundError{
			Source: path,
		}
	}
	return obj.(*TerragruntRecord), nil
}

func terragruntRecordCopyByPath(txn *memdb.Txn, path string) (*TerragruntRecord, error) {
	record, err := terragruntRecordByPath(txn, path)
	if err != nil {
		return nil, err
	}

	return record.Copy(), nil
}

func (s *TerragruntStore) UpdateParsedFiles(path string, files ast.Files, languages ast.Languages, fErr error) error {
	txn := s.db.Txn(true)
	defer txn.Abort()

	record, err := terragruntRecordCopyByPath(txn, path)
	if err != nil {
		return err
	}

	record.ParsedFiles = files
	record.Languages = languages
	record.FilesParsingErr = fErr

	err = txn.Insert(s.tableName, record)
	if err != nil {
		return err
	}

	txn.Commit()
	return nil
}

func (s *TerragruntStore) UpdateDiagnostics(path string, source globalAst.DiagnosticSource, diags ast.Diags) error {
	txn := s.db.Txn(true)
	txn.Defer(func() {
		s.SetDiagnosticsState(path, source, op.OpStateLoaded)
	})
	defer txn.Abort()

	oldRecord, err := terragruntRecordByPath(txn, path)
	if err != nil {
		return err
	}

	record := oldRecord.Copy()
	if record.Diagnostics == nil {
		record.Diagnostics = make(ast.SourceDiags)
	}
	record.Diagnostics[source] = diags

	err = txn.Insert(s.tableName, record)
	if err != nil {
		return err
	}

	err = s.queueRecordChange(oldRecord, record)
	if err != nil {
		return err
	}

	txn.Commit()
	return nil
}

func (s *TerragruntStore) SetDiagnosticsState(path string, source globalAst.DiagnosticSource, state op.OpState) error {
	txn := s.db.Txn(true)
	defer txn.Abort()

	record, err := terragruntRecordCopyByPath(txn, path)
	if err != nil {
		return err
	}
	record.DiagnosticsState[source] = state

	err = txn.Insert(s.tableName, record)
	if err != nil {
		return err
	}

	txn.Commit()
	return nil
}

func (s *TerragruntStore) SetReferencesState(path string, state op.OpState) error {
	txn := s.db.Txn(true)
	defer txn.Abort()

	record, err := terragruntRecordCopyByPath(txn, path)
	if err != nil {
		return err
	}

	record.RefOriginsState = state
	err = txn.Insert(s.tableName, record)
	if err != nil {
		return err
	}

	txn.Commit()
	return nil
}

func (s *TerragruntStore) UpdateReferences(path string, origins reference.Origins, targets reference.Targets, rErr error) error {
	txn := s.db.Txn(true)
	txn.Defer(func() {
		s.SetReferencesState(path, op.OpStateLoaded)
	})
	defer txn.Abort()

	oldRecord, err := terragruntRecordByPath(txn, path)
	if err != nil {
		return err
	}

	record := oldRecord.Copy()
	record.RefOrigins = origins
	record.RefTargets = targets
	record.RefErr = rErr

	err = txn.Insert(s.tableName, record)
	if err != nil {
		return err
	}

	err = s.queueRecordChange(oldRecord, record)
	if err != nil {
		return err
	}

	txn.Commit()
	return nil
}

func (s *TerragruntStore) queueRecordChange(oldRecord, newRecord *TerragruntRecord) error {
	changes := globalState.Changes{}

	oldDiags, newDiags := 0, 0
	if oldRecord != nil {
		oldDiags = oldRecord.Diagnostics.Count()
	}
	if newRecord != nil {
		newDiags = newRecord.Diagnostics.Count()
	}
	// Comparing diagnostics accurately could be expensive
	// so we just treat any non-empty diags as a change
	if oldDiags > 0 || newDiags > 0 {
		changes.Diagnostics = true
	}

	oldOrigins, newOrigins := 0, 0
	if oldRecord != nil {
		oldOrigins = len(oldRecord.RefOrigins)
	}
	if newRecord != nil {
		newOrigins = len(newRecord.RefOrigins)
	}
	if oldOrigins != newOrigins {
		changes.ReferenceOrigins = true
	}

	var dir document.DirHandle
	if oldRecord != nil {
		dir = document.DirHandleFromPath(oldRecord.Path())
	} else {
		dir = document.DirHandleFromPath(newRecord.Path())
	}

	return s.changeStore.QueueChange(dir, changes)
}
