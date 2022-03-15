// Copyright 2021 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package creation

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb/durable"
	"github.com/dolthub/dolt/go/libraries/doltcore/schema"
	"github.com/dolthub/dolt/go/libraries/doltcore/table/editor"
	"github.com/dolthub/dolt/go/store/pool"
	"github.com/dolthub/dolt/go/store/types"
	"github.com/dolthub/dolt/go/store/val"
)

type CreateIndexReturn struct {
	NewTable *doltdb.Table
	Sch      schema.Schema
	OldIndex schema.Index
	NewIndex schema.Index
}

// CreateIndex creates the given index on the given table with the given schema. Returns the updated table, updated schema, and created index.
func CreateIndex(
	ctx context.Context,
	table *doltdb.Table,
	indexName string,
	columns []string,
	isUnique bool,
	isUserDefined bool,
	comment string,
	opts editor.Options,
) (*CreateIndexReturn, error) {
	sch, err := table.GetSchema(ctx)
	if err != nil {
		return nil, err
	}

	// get the real column names as CREATE INDEX columns are case-insensitive
	var realColNames []string
	allTableCols := sch.GetAllCols()
	for _, indexCol := range columns {
		tableCol, ok := allTableCols.GetByNameCaseInsensitive(indexCol)
		if !ok {
			return nil, fmt.Errorf("column `%s` does not exist for the table", indexCol)
		}
		realColNames = append(realColNames, tableCol.Name)
	}

	if indexName == "" {
		indexName = strings.Join(realColNames, "")
		_, ok := sch.Indexes().GetByNameCaseInsensitive(indexName)
		var i int
		for ok {
			i++
			indexName = fmt.Sprintf("%s_%d", strings.Join(realColNames, ""), i)
			_, ok = sch.Indexes().GetByNameCaseInsensitive(indexName)
		}
	}
	if !doltdb.IsValidIndexName(indexName) {
		return nil, fmt.Errorf("invalid index name `%s` as they must match the regular expression %s", indexName, doltdb.IndexNameRegexStr)
	}

	// if an index was already created for the column set but was not generated by the user then we replace it
	replacingIndex := false
	existingIndex, ok := sch.Indexes().GetIndexByColumnNames(realColNames...)
	if ok && !existingIndex.IsUserDefined() {
		replacingIndex = true
		_, err = sch.Indexes().RemoveIndex(existingIndex.Name())
		if err != nil {
			return nil, err
		}
	}

	// create the index metadata, will error if index names are taken or an index with the same columns in the same order exists
	index, err := sch.Indexes().AddIndexByColNames(
		indexName,
		realColNames,
		schema.IndexProperties{
			IsUnique:      isUnique,
			IsUserDefined: isUserDefined,
			Comment:       comment,
		},
	)
	if err != nil {
		return nil, err
	}

	// update the table schema with the new index
	newTable, err := table.UpdateSchema(ctx, sch)
	if err != nil {
		return nil, err
	}

	if replacingIndex { // verify that the pre-existing index data is valid
		newTable, err = newTable.RenameIndexRowData(ctx, existingIndex.Name(), index.Name())
		if err != nil {
			return nil, err
		}
		err = newTable.VerifyIndexRowData(ctx, index.Name())
		if err != nil {
			return nil, err
		}
	} else { // set the index row data and get a new root with the updated table
		indexRows, err := BuildSecondaryIndex(ctx, newTable, index, opts)
		if err != nil {
			return nil, err
		}

		newTable, err = newTable.SetIndexRows(ctx, index.Name(), indexRows)
		if err != nil {
			return nil, err
		}
	}
	return &CreateIndexReturn{
		NewTable: newTable,
		Sch:      sch,
		OldIndex: existingIndex,
		NewIndex: index,
	}, nil
}

func BuildSecondaryIndex(ctx context.Context, tbl *doltdb.Table, idx schema.Index, opts editor.Options) (durable.Index, error) {
	switch tbl.Format() {
	case types.Format_LD_1, types.Format_DOLT_DEV:
		m, err := editor.RebuildIndex(ctx, tbl, idx.Name(), opts)
		if err != nil {
			return nil, err
		}
		return durable.IndexFromNomsMap(m, tbl.ValueReadWriter()), nil

	case types.Format_DOLT_1:
		return BuildSecondaryProllyIndex(ctx, tbl, idx)

	default:
		return nil, fmt.Errorf("unknown NomsBinFormat")
	}
}

func BuildSecondaryProllyIndex(ctx context.Context, tbl *doltdb.Table, idx schema.Index) (durable.Index, error) {
	sch, err := tbl.GetSchema(ctx)
	if err != nil {
		return nil, err
	}

	empty, err := durable.NewEmptyIndex(ctx, tbl.ValueReadWriter(), idx.Schema())
	if err != nil {
		return nil, err
	}
	secondary := durable.ProllyMapFromIndex(empty)

	m, err := tbl.GetRowData(ctx)
	if err != nil {
		return nil, err
	}
	primary := durable.ProllyMapFromIndex(m)

	iter, err := primary.IterAll(ctx)
	if err != nil {
		return nil, err
	}
	pkLen := sch.GetPKCols().Size()

	// create a key builder for index key tuples
	kd, _ := secondary.Descriptors()
	keyBld := val.NewTupleBuilder(kd)
	keyMap := getIndexKeyMapping(sch, idx)

	mut := secondary.Mutate()
	for {
		k, v, err := iter.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		for to := range keyMap {
			from := keyMap.MapOrdinal(to)
			if from < pkLen {
				keyBld.PutRaw(to, k.GetField(from))
			} else {
				from -= pkLen
				keyBld.PutRaw(to, v.GetField(from))
			}
		}

		// todo(andy): build permissive?
		idxKey := keyBld.Build(sharePool)
		idxVal := val.EmptyTuple

		// todo(andy): periodic flushing
		if err = mut.Put(ctx, idxKey, idxVal); err != nil {
			return nil, err
		}
	}

	secondary, err = mut.Map(ctx)
	if err != nil {
		return nil, err
	}

	return durable.IndexFromProllyMap(secondary), nil
}

func getIndexKeyMapping(sch schema.Schema, idx schema.Index) (m val.OrdinalMapping) {
	m = make(val.OrdinalMapping, len(idx.AllTags()))

	for i, tag := range idx.AllTags() {
		j, ok := sch.GetPKCols().TagToIdx[tag]
		if !ok {
			j = sch.GetNonPKCols().TagToIdx[tag]
			j += sch.GetPKCols().Size()
		}
		m[i] = j
	}

	return
}

var sharePool = pool.NewBuffPool()
