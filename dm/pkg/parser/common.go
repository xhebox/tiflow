// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package parser

import (
	"bytes"

	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/format"
	_ "github.com/pingcap/tidb/pkg/types/parser_driver" // for import parser driver
	"github.com/pingcap/tidb/pkg/util/filter"
	"github.com/pingcap/tiflow/dm/pkg/conn"
	"github.com/pingcap/tiflow/dm/pkg/log"
	"github.com/pingcap/tiflow/dm/pkg/terror"
	"github.com/pingcap/tiflow/dm/pkg/utils"
	"go.uber.org/zap"
)

const (
	// SingleRenameTableNameNum stands for number of TableNames in a single table renaming. it's 2 after
	// https://github.com/pingcap/parser/pull/1021
	SingleRenameTableNameNum = 2
)

// Parse wraps parser.Parse(), makes `parser` suitable for dm.
func Parse(p *parser.Parser, sql, charset, collation string) (stmt []ast.StmtNode, err error) {
	stmts, warnings, err := p.Parse(sql, charset, collation)
	if len(warnings) > 0 {
		log.L().Warn("parse statement", zap.String("sql", sql), zap.Errors("warning messages", warnings))
	}

	return stmts, terror.ErrParseSQL.Delegate(err)
}

// ref: https://github.com/pingcap/tidb/blob/09feccb529be2830944e11f5fed474020f50370f/server/sql_info_fetcher.go#L46
type tableNameExtractor struct {
	curDB  string
	flavor conn.LowerCaseTableNamesFlavor
	names  []*filter.Table
}

func (tne *tableNameExtractor) Enter(in ast.Node) (ast.Node, bool) {
	if _, ok := in.(*ast.ReferenceDef); ok {
		return in, true
	}
	if t, ok := in.(*ast.TableName); ok {
		var tb *filter.Table
		if tne.flavor == conn.LCTableNamesSensitive {
			tb = &filter.Table{Schema: t.Schema.O, Name: t.Name.O}
		} else {
			tb = &filter.Table{Schema: t.Schema.L, Name: t.Name.L}
		}

		if tb.Schema == "" {
			tb.Schema = tne.curDB
		}
		tne.names = append(tne.names, tb)
		return in, true
	}
	return in, false
}

func (tne *tableNameExtractor) Leave(in ast.Node) (ast.Node, bool) {
	return in, true
}

// FetchDDLTables returns tables in ddl the result contains many tables.
// Because we use visitor pattern, first tableName is always upper-most table in ast
// specifically, for `create table like` DDL, result contains [sourceTable, sourceRefTable]
// for rename table ddl, result contains [old1, new1, old2, new2, old3, new3, ...] because of TiDB parser
// for other DDL, order of tableName is the node visit order.
func FetchDDLTables(schema string, stmt ast.StmtNode, flavor conn.LowerCaseTableNamesFlavor) ([]*filter.Table, error) {
	switch stmt.(type) {
	case ast.DDLNode:
	default:
		return nil, terror.ErrUnknownTypeDDL.Generate(stmt)
	}

	// special cases: schema related SQLs doesn't have tableName
	// todo: pass .O or .L of table name depends on flavor
	switch v := stmt.(type) {
	case *ast.AlterDatabaseStmt:
		return []*filter.Table{genTableName(v.Name.O, "")}, nil
	case *ast.CreateDatabaseStmt:
		return []*filter.Table{genTableName(v.Name.O, "")}, nil
	case *ast.DropDatabaseStmt:
		return []*filter.Table{genTableName(v.Name.O, "")}, nil
	}

	e := &tableNameExtractor{
		curDB:  schema,
		flavor: flavor,
		names:  make([]*filter.Table, 0),
	}
	stmt.Accept(e)

	return e.names, nil
}

type tableRenameVisitor struct {
	targetNames []*filter.Table
	i           int
	hasErr      bool
}

func (v *tableRenameVisitor) Enter(in ast.Node) (ast.Node, bool) {
	if v.hasErr {
		return in, true
	}
	if _, ok := in.(*ast.ReferenceDef); ok {
		return in, true
	}
	if t, ok := in.(*ast.TableName); ok {
		if v.i >= len(v.targetNames) {
			v.hasErr = true
			return in, true
		}
		t.Schema = ast.NewCIStr(v.targetNames[v.i].Schema)
		t.Name = ast.NewCIStr(v.targetNames[v.i].Name)
		v.i++
		return in, true
	}
	return in, false
}

func (v *tableRenameVisitor) Leave(in ast.Node) (ast.Node, bool) {
	if v.hasErr {
		return in, false
	}
	return in, true
}

// RenameDDLTable renames tables in ddl by given `targetTables`
// argument `targetTables` is same with return value of FetchDDLTables
// returned DDL is formatted like StringSingleQuotes, KeyWordUppercase and NameBackQuotes.
func RenameDDLTable(stmt ast.StmtNode, targetTables []*filter.Table) (string, error) {
	switch stmt.(type) {
	case ast.DDLNode:
	default:
		return "", terror.ErrUnknownTypeDDL.Generate(stmt)
	}

	switch v := stmt.(type) {
	case *ast.AlterDatabaseStmt:
		v.Name = ast.NewCIStr(targetTables[0].Schema)
	case *ast.CreateDatabaseStmt:
		v.Name = ast.NewCIStr(targetTables[0].Schema)
	case *ast.DropDatabaseStmt:
		v.Name = ast.NewCIStr(targetTables[0].Schema)
	default:
		visitor := &tableRenameVisitor{
			targetNames: targetTables,
		}
		stmt.Accept(visitor)
		if visitor.hasErr {
			return "", terror.ErrRewriteSQL.Generate(stmt, targetTables)
		}
	}

	var b []byte
	bf := bytes.NewBuffer(b)
	err := stmt.Restore(&format.RestoreCtx{
		Flags: format.DefaultRestoreFlags | format.RestoreTiDBSpecialComment | format.RestoreStringWithoutDefaultCharset,
		In:    bf,
	})
	if err != nil {
		return "", terror.ErrRestoreASTNode.Delegate(err)
	}

	return bf.String(), nil
}

// SplitDDL splits multiple operations in one DDL statement into multiple DDL statements
// returned DDL is formatted like StringSingleQuotes, KeyWordUppercase and NameBackQuotes
// if fail to restore, it would not restore the value of `stmt` (it changes it's values if `stmt` is one of  DropTableStmt, RenameTableStmt, AlterTableStmt).
func SplitDDL(stmt ast.StmtNode, schema string) (sqls []string, err error) {
	var (
		schemaName = ast.NewCIStr(schema) // fill schema name
		bf         = new(bytes.Buffer)
		ctx        = &format.RestoreCtx{
			Flags: format.DefaultRestoreFlags | format.RestoreTiDBSpecialComment | format.RestoreStringWithoutDefaultCharset,
			In:    bf,
		}
	)

	switch v := stmt.(type) {
	case *ast.CreateSequenceStmt:
	case *ast.AlterSequenceStmt:
	case *ast.DropSequenceStmt:
	case *ast.AlterDatabaseStmt:
		if v.AlterDefaultDatabase {
			v.AlterDefaultDatabase = false
			v.Name = schemaName
		}
	case *ast.CreateDatabaseStmt:
		v.IfNotExists = true
	case *ast.DropDatabaseStmt:
		v.IfExists = true
	case *ast.DropTableStmt:
		v.IfExists = true

		tables := v.Tables
		for _, t := range tables {
			if t.Schema.O == "" {
				t.Schema = schemaName
			}

			v.Tables = []*ast.TableName{t}
			bf.Reset()
			err = stmt.Restore(ctx)
			if err != nil {
				v.Tables = tables
				return nil, terror.ErrRestoreASTNode.Delegate(err)
			}

			sqls = append(sqls, bf.String())
		}
		v.Tables = tables

		return sqls, nil
	case *ast.CreateTableStmt:
		v.IfNotExists = true
		if v.Table.Schema.O == "" {
			v.Table.Schema = schemaName
		}

		if v.ReferTable != nil && v.ReferTable.Schema.O == "" {
			v.ReferTable.Schema = schemaName
		}
	case *ast.TruncateTableStmt:
		if v.Table.Schema.O == "" {
			v.Table.Schema = schemaName
		}
	case *ast.DropIndexStmt:
		v.IfExists = true
		if v.Table.Schema.O == "" {
			v.Table.Schema = schemaName
		}
	case *ast.CreateIndexStmt:
		if v.Table.Schema.O == "" {
			v.Table.Schema = schemaName
		}
	case *ast.RenameTableStmt:
		t2ts := v.TableToTables
		for _, t2t := range t2ts {
			if t2t.OldTable.Schema.O == "" {
				t2t.OldTable.Schema = schemaName
			}
			if t2t.NewTable.Schema.O == "" {
				t2t.NewTable.Schema = schemaName
			}

			v.TableToTables = []*ast.TableToTable{t2t}

			bf.Reset()
			err = stmt.Restore(ctx)
			if err != nil {
				v.TableToTables = t2ts
				return nil, terror.ErrRestoreASTNode.Delegate(err)
			}

			sqls = append(sqls, bf.String())
		}
		v.TableToTables = t2ts

		return sqls, nil
	case *ast.AlterTableStmt:
		specs := v.Specs
		table := v.Table

		if v.Table.Schema.O == "" {
			v.Table.Schema = schemaName
		}

		for _, spec := range specs {
			if spec.Tp == ast.AlterTableRenameTable {
				if spec.NewTable.Schema.O == "" {
					spec.NewTable.Schema = schemaName
				}
			}

			v.Specs = []*ast.AlterTableSpec{spec}

			// handle `alter table t1 add column (c1 int, c2 int)`
			if spec.Tp == ast.AlterTableAddColumns && len(spec.NewColumns) > 1 {
				columns := spec.NewColumns
				spec.Position = &ast.ColumnPosition{
					Tp: ast.ColumnPositionNone, // otherwise restore will become "alter table t1 add column (c1 int)"
				}
				for _, c := range columns {
					spec.NewColumns = []*ast.ColumnDef{c}
					bf.Reset()
					err = stmt.Restore(ctx)
					if err != nil {
						v.Specs = specs
						v.Table = table
						return nil, terror.ErrRestoreASTNode.Delegate(err)
					}
					sqls = append(sqls, bf.String())
				}
				// we have restore SQL for every columns, skip below general restoring and continue on next spec
				continue
			}

			bf.Reset()
			err = stmt.Restore(ctx)
			if err != nil {
				v.Specs = specs
				v.Table = table
				return nil, terror.ErrRestoreASTNode.Delegate(err)
			}
			sqls = append(sqls, bf.String())

			if spec.Tp == ast.AlterTableRenameTable {
				v.Table = spec.NewTable
			}
		}
		v.Specs = specs
		v.Table = table

		return sqls, nil
	default:
		return nil, terror.ErrUnknownTypeDDL.Generate(stmt)
	}

	bf.Reset()
	err = stmt.Restore(ctx)
	if err != nil {
		return nil, terror.ErrRestoreASTNode.Delegate(err)
	}
	sqls = append(sqls, bf.String())

	return sqls, nil
}

func genTableName(schema string, table string) *filter.Table {
	return &filter.Table{Schema: schema, Name: table}
}

// CheckIsDDL checks input SQL whether is a valid DDL statement.
func CheckIsDDL(sql string, p *parser.Parser) bool {
	// fast path for begin/comit
	if sql == "BEGIN" || sql == "COMMIT" {
		return false
	}
	sql = utils.TrimCtrlChars(sql)

	if utils.IsBuildInSkipDDL(sql) {
		return false
	}

	// if parse error, treat it as not a DDL
	stmts, err := Parse(p, sql, "", "")
	if err != nil || len(stmts) == 0 {
		return false
	}

	stmt := stmts[0]
	switch stmt.(type) {
	case ast.DDLNode:
		return true
	default:
		// other thing this like `BEGIN`
		return false
	}
}
