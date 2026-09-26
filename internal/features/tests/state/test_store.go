// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"log"

	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/hcl-lang/reference"
	"github.com/opentofu/tofu-ls/internal/document"
	"github.com/opentofu/tofu-ls/internal/features/tests/ast"
	globalState "github.com/opentofu/tofu-ls/internal/state"
	globalAst "github.com/opentofu/tofu-ls/internal/tofu/ast"
	op "github.com/opentofu/tofu-ls/internal/tofu/module/operation"
)

type TestStore struct {
	db        *memdb.MemDB
	tableName string
	logger    *log.Logger

	changeStore *globalState.ChangeStore
}

func (s *TestStore) SetLogger(logger *log.Logger) {
	s.logger = logger
}

func (s *TestStore) Add(path string) error {
	txn := s.db.Txn(true)
	defer txn.Abort()

	err := s.add(txn, path)
	if err != nil {
		return err
	}
	txn.Commit()

	return nil
}

func (s *TestStore) add(txn *memdb.Txn, path string) error {
	obj, err := txn.First(s.tableName, "id", path)
	if err != nil {
		return err
	}
	if obj != nil {
		return &globalState.AlreadyExistsError{
			Idx: path,
		}
	}

	record := newTestRecord(path)
	err = txn.Insert(s.tableName, record)
	if err != nil {
		return err
	}

	return s.queueRecordChange(nil, record)
}

func (s *TestStore) AddIfNotExists(path string) error {
	txn := s.db.Txn(true)
	defer txn.Abort()

	_, err := testRecordByPath(txn, path)
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

func (s *TestStore) Remove(path string) error {
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

	err = s.queueRecordChange(oldObj.(*TestRecord), nil)
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

func (s *TestStore) List() ([]*TestRecord, error) {
	txn := s.db.Txn(false)

	it, err := txn.Get(s.tableName, "id")
	if err != nil {
		return nil, err
	}

	records := make([]*TestRecord, 0)
	for item := it.Next(); item != nil; item = it.Next() {
		records = append(records, item.(*TestRecord))
	}

	return records, nil
}

func (s *TestStore) Exists(path string) bool {
	txn := s.db.Txn(false)

	obj, err := txn.First(s.tableName, "id", path)
	if err != nil {
		return false
	}

	return obj != nil
}

func (s *TestStore) TestRecordByPath(path string) (*TestRecord, error) {
	txn := s.db.Txn(false)

	return testRecordByPath(txn, path)
}

func testRecordByPath(txn *memdb.Txn, path string) (*TestRecord, error) {
	obj, err := txn.First(testTableName, "id", path)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, &globalState.RecordNotFoundError{
			Source: path,
		}
	}
	return obj.(*TestRecord), nil
}

func testRecordCopyByPath(txn *memdb.Txn, path string) (*TestRecord, error) {
	record, err := testRecordByPath(txn, path)
	if err != nil {
		return nil, err
	}

	return record.Copy(), nil
}

func (s *TestStore) UpdateParsedFiles(path string, files ast.Files, fErr error) error {
	txn := s.db.Txn(true)
	defer txn.Abort()

	record, err := testRecordCopyByPath(txn, path)
	if err != nil {
		return err
	}

	record.ParsedFiles = files
	record.FilesParsingErr = fErr

	err = txn.Insert(s.tableName, record)
	if err != nil {
		return err
	}

	txn.Commit()
	return nil
}

func (s *TestStore) UpdateDiagnostics(path string, source globalAst.DiagnosticSource, diags ast.Diags) error {
	txn := s.db.Txn(true)
	txn.Defer(func() {
		s.SetDiagnosticsState(path, source, op.OpStateLoaded)
	})
	defer txn.Abort()

	oldRecord, err := testRecordByPath(txn, path)
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

func (s *TestStore) SetDiagnosticsState(path string, source globalAst.DiagnosticSource, state op.OpState) error {
	txn := s.db.Txn(true)
	defer txn.Abort()

	record, err := testRecordCopyByPath(txn, path)
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

func (s *TestStore) SetReferencesState(path string, state op.OpState) error {
	txn := s.db.Txn(true)
	defer txn.Abort()

	record, err := testRecordCopyByPath(txn, path)
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

func (s *TestStore) UpdateReferences(path string, origins reference.Origins, targets reference.Targets, rErr error) error {
	txn := s.db.Txn(true)
	txn.Defer(func() {
		s.SetReferencesState(path, op.OpStateLoaded)
	})
	defer txn.Abort()

	oldRecord, err := testRecordByPath(txn, path)
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

func (s *TestStore) queueRecordChange(oldRecord, newRecord *TestRecord) error {
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
