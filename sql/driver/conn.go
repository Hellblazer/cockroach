// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Peter Mattis (peter@cockroachlabs.com)

package driver

import (
	"bytes"
	"database/sql/driver"
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroach/client"
	"github.com/cockroachdb/cockroach/keys"
	"github.com/cockroachdb/cockroach/sql/parser"
	"github.com/cockroachdb/cockroach/structured"
)

// TODO(pmattis):
//
// - This file contains the experimental Cockroach sql driver. The driver
//   currently parses SQL and executes key/value operations in order to execute
//   the SQL. The execution will fairly quickly migrate to the server with the
//   driver performing RPCs.
//
// - Flesh out basic insert, update, delete and select operations.
//
// - Figure out transaction story.

// conn implements the sql/driver.Conn interface. Note that conn is assumed to
// be stateful and is not used concurrently by multiple goroutines; See
// https://golang.org/pkg/database/sql/driver/#Conn.
type conn struct {
	db       *client.DB
	database string
}

func (c *conn) Close() error {
	return nil
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	s, err := parser.Parse(query)
	if err != nil {
		return nil, err
	}
	return &stmt{conn: c, stmt: s}, nil
}

func (c *conn) Exec(query string, args []driver.Value) (driver.Result, error) {
	stmt, err := parser.Parse(query)
	if err != nil {
		return nil, err
	}
	return c.exec(stmt, args)
}

func (c *conn) Query(query string, args []driver.Value) (driver.Rows, error) {
	stmt, err := parser.Parse(query)
	if err != nil {
		return nil, err
	}
	return c.query(stmt, args)
}

func (c *conn) Begin() (driver.Tx, error) {
	return &tx{conn: c}, nil
}

func (c *conn) exec(stmt parser.Statement, args []driver.Value) (driver.Result, error) {
	rows, err := c.query(stmt, args)
	if err != nil {
		return nil, err
	}
	return driver.RowsAffected(len(rows.rows)), nil
}

func (c *conn) query(stmt parser.Statement, args []driver.Value) (*rows, error) {
	switch p := stmt.(type) {
	case *parser.CreateDatabase:
		return c.CreateDatabase(p, args)
	case *parser.CreateTable:
		return c.CreateTable(p, args)
	case *parser.Delete:
		return c.Delete(p, args)
	case *parser.Insert:
		return c.Insert(p, args)
	case *parser.Select:
		return c.Select(p, args)
	case *parser.ShowColumns:
		return c.ShowColumns(p, args)
	case *parser.ShowDatabases:
		return c.ShowDatabases(p, args)
	case *parser.ShowIndex:
		return c.ShowIndex(p, args)
	case *parser.ShowTables:
		return c.ShowTables(p, args)
	case *parser.Update:
		return c.Update(p, args)
	case *parser.Use:
		return c.Use(p, args)

	case *parser.AlterTable:
	case *parser.AlterView:
	case *parser.CreateIndex:
	case *parser.CreateView:
	case *parser.DropDatabase:
	case *parser.DropIndex:
	case *parser.DropTable:
	case *parser.DropView:
	case *parser.RenameTable:
	case *parser.Set:
	case *parser.TruncateTable:
	case *parser.Union:
		// Various unimplemented statements.

	default:
		return nil, fmt.Errorf("unknown statement type: %T", stmt)
	}

	return nil, fmt.Errorf("TODO(pmattis): unimplemented: %T %s", stmt, stmt)
}

func (c *conn) CreateDatabase(p *parser.CreateDatabase, args []driver.Value) (*rows, error) {
	if p.Name == "" {
		return nil, fmt.Errorf("empty database name")
	}

	nameKey := keys.MakeNameMetadataKey(structured.RootNamespaceID, strings.ToLower(p.Name))
	if gr, err := c.db.Get(nameKey); err != nil {
		return nil, err
	} else if gr.Exists() {
		if p.IfNotExists {
			return &rows{}, nil
		}
		return nil, fmt.Errorf("database \"%s\" already exists", p.Name)
	}
	ir, err := c.db.Inc(keys.DescIDGenerator, 1)
	if err != nil {
		return nil, err
	}
	nsID := uint32(ir.ValueInt() - 1)
	if err := c.db.CPut(nameKey, nsID, nil); err != nil {
		// TODO(pmattis): Need to handle if-not-exists here as well.
		return nil, err
	}
	return &rows{}, nil
}

func (c *conn) CreateTable(p *parser.CreateTable, args []driver.Value) (*rows, error) {
	if err := c.normalizeTableName(p.Name); err != nil {
		return nil, err
	}

	dbID, err := c.lookupDatabase(p.Name.Qualifier)
	if err != nil {
		return nil, err
	}

	schema, err := makeSchema(p)
	if err != nil {
		return nil, err
	}
	desc := structured.TableDescFromSchema(schema)
	if err := structured.ValidateTableDesc(desc); err != nil {
		return nil, err
	}

	nameKey := keys.MakeNameMetadataKey(dbID, p.Name.Name)

	// This isn't strictly necessary as the conditional put below will fail if
	// the key already exists, but it seems good to avoid the table ID allocation
	// in most cases when the table already exists.
	if gr, err := c.db.Get(nameKey); err != nil {
		return nil, err
	} else if gr.Exists() {
		if p.IfNotExists {
			return &rows{}, nil
		}
		return nil, fmt.Errorf("table \"%s\" already exists", p.Name.Name)
	}

	ir, err := c.db.Inc(keys.DescIDGenerator, 1)
	if err != nil {
		return nil, err
	}
	desc.ID = uint32(ir.ValueInt() - 1)

	// TODO(pmattis): Be cognizant of error messages when this is ported to the
	// server. The error currently returned below is likely going to be difficult
	// to interpret.
	err = c.db.Txn(func(txn *client.Txn) error {
		descKey := keys.MakeDescMetadataKey(desc.ID)
		b := &client.Batch{}
		b.CPut(nameKey, descKey, nil)
		b.Put(descKey, &desc)
		return txn.Commit(b)
	})
	if err != nil {
		// TODO(pmattis): Need to handle if-not-exists here as well.
		return nil, err
	}
	return &rows{}, nil
}

func (c *conn) Delete(p *parser.Delete, args []driver.Value) (*rows, error) {
	return nil, fmt.Errorf("TODO(pmattis): unimplemented: %T %s", p, p)
}

func (c *conn) Insert(p *parser.Insert, args []driver.Value) (*rows, error) {
	return nil, fmt.Errorf("TODO(pmattis): unimplemented: %T %s", p, p)
}

func (c *conn) Select(p *parser.Select, args []driver.Value) (*rows, error) {
	return nil, fmt.Errorf("TODO(pmattis): unimplemented: %T %s", p, p)
}

func (c *conn) ShowColumns(p *parser.ShowColumns, args []driver.Value) (*rows, error) {
	desc, err := c.getTableDesc(p.Name)
	if err != nil {
		return nil, err
	}

	schema := structured.TableSchemaFromDesc(*desc)

	// TODO(pmattis): This output doesn't match up with MySQL. Should it?
	r := &rows{
		columns: []string{"Field", "Type", "Null"},
		rows:    make([]row, len(schema.Columns)),
		pos:     -1,
	}

	for i, col := range schema.Columns {
		t := make(row, len(r.columns))
		t[0] = col.Name
		t[1] = col.Type.SQLString()
		t[2] = col.Nullable
		r.rows[i] = t
	}

	return r, nil
}

func (c *conn) ShowDatabases(p *parser.ShowDatabases, args []driver.Value) (*rows, error) {
	prefix := keys.MakeNameMetadataKey(structured.RootNamespaceID, "")
	sr, err := c.db.Scan(prefix, prefix.PrefixEnd(), 0)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(sr))
	for i, row := range sr {
		names[i] = string(bytes.TrimPrefix(row.Key, prefix))
	}
	return newSingleColumnRows("database", names), nil
}

func (c *conn) ShowIndex(p *parser.ShowIndex, args []driver.Value) (*rows, error) {
	desc, err := c.getTableDesc(p.Name)
	if err != nil {
		return nil, err
	}

	schema := structured.TableSchemaFromDesc(*desc)

	// TODO(pmattis): This output doesn't match up with MySQL. Should it?
	r := &rows{
		columns: []string{"Table", "Name", "Unique", "Seq", "Column"},
		pos:     -1,
	}
	for _, index := range schema.Indexes {
		for j, col := range index.ColumnNames {
			t := make(row, len(r.columns))
			t[0] = p.Name.Name
			t[1] = index.Name
			t[2] = index.Unique
			t[3] = j + 1
			t[4] = col
			r.rows = append(r.rows, t)
		}
	}

	return r, nil
}

func (c *conn) ShowTables(p *parser.ShowTables, args []driver.Value) (*rows, error) {
	if p.Name == "" {
		if c.database == "" {
			return nil, fmt.Errorf("no database specified")
		}
		p.Name = c.database
	}
	dbID, err := c.lookupDatabase(p.Name)
	if err != nil {
		return nil, err
	}
	prefix := keys.MakeNameMetadataKey(dbID, "")
	sr, err := c.db.Scan(prefix, prefix.PrefixEnd(), 0)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(sr))
	for i, row := range sr {
		names[i] = string(bytes.TrimPrefix(row.Key, prefix))
	}
	return newSingleColumnRows("tables", names), nil
}

func (c *conn) Update(p *parser.Update, args []driver.Value) (*rows, error) {
	return nil, fmt.Errorf("TODO(pmattis): unimplemented: %T %s", p, p)
}

func (c *conn) Use(p *parser.Use, args []driver.Value) (*rows, error) {
	c.database = p.Name
	return &rows{}, nil
}

func (c *conn) getTableDesc(name *parser.TableName) (*structured.TableDescriptor, error) {
	if err := c.normalizeTableName(name); err != nil {
		return nil, err
	}
	dbID, err := c.lookupDatabase(name.Qualifier)
	if err != nil {
		return nil, err
	}
	gr, err := c.db.Get(keys.MakeNameMetadataKey(dbID, name.Name))
	if err != nil {
		return nil, err
	}
	if !gr.Exists() {
		return nil, fmt.Errorf("table \"%s\" does not exist", name)
	}
	descKey := gr.ValueBytes()
	desc := structured.TableDescriptor{}
	if err := c.db.GetProto(descKey, &desc); err != nil {
		return nil, err
	}
	if err := structured.ValidateTableDesc(desc); err != nil {
		return nil, err
	}
	return &desc, nil
}

func (c *conn) normalizeTableName(name *parser.TableName) error {
	if name.Qualifier == "" {
		if c.database == "" {
			return fmt.Errorf("no database specified")
		}
		name.Qualifier = c.database
	}
	if name.Name == "" {
		return fmt.Errorf("empty table name: %s", name.Name)
	}
	return nil
}

func (c *conn) lookupDatabase(name string) (uint32, error) {
	nameKey := keys.MakeNameMetadataKey(structured.RootNamespaceID, name)
	gr, err := c.db.Get(nameKey)
	if err != nil {
		return 0, err
	} else if !gr.Exists() {
		return 0, fmt.Errorf("database \"%s\" does not exist", name)
	}
	return uint32(gr.ValueInt()), nil
}
