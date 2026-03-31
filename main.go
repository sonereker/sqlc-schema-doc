package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"github.com/sqlc-dev/plugin-sdk-go/codegen"
	"github.com/sqlc-dev/plugin-sdk-go/plugin"
)

type options struct {
	MigrationsDir string   `json:"migrations_dir"`
	Exclude       []string `json:"exclude"`
}

type column struct {
	Name       string
	Type       string
	NotNull    bool
	IsArray    bool
	PrimaryKey bool
	Unique     bool
	Default    string
	References string
}

type table struct {
	Name    string
	Columns []column
}

type enum struct {
	Name   string
	Values []string
}

func main() {
	codegen.Run(generate)
}

func generate(_ context.Context, req *plugin.GenerateRequest) (*plugin.GenerateResponse, error) {
	var opts options
	if len(req.PluginOptions) > 0 {
		if err := json.Unmarshal(req.PluginOptions, &opts); err != nil {
			return nil, fmt.Errorf("sqlc-schema-doc: invalid plugin options: %w", err)
		}
	}

	if opts.MigrationsDir == "" {
		return nil, fmt.Errorf("sqlc-schema-doc: migrations_dir is required in plugin options")
	}

	// Default excludes
	if len(opts.Exclude) == 0 {
		opts.Exclude = []string{"river_"}
	}

	// Resolve migrations directory relative to the sqlc config working directory
	migrationsDir := opts.MigrationsDir

	// Find all .up.sql files
	pattern := filepath.Join(migrationsDir, "*.up.sql")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("sqlc-schema-doc: failed to glob migrations: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("sqlc-schema-doc: no *.up.sql files found in %s", migrationsDir)
	}

	sort.Strings(files)

	// Read and concatenate all migration files
	var sqlBuilder strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("sqlc-schema-doc: failed to read %s: %w", f, err)
		}
		sqlBuilder.Write(data)
		sqlBuilder.WriteString("\n")
	}

	// Parse concatenated SQL
	result, err := pg_query.Parse(sqlBuilder.String())
	if err != nil {
		// If full parse fails, try parsing files individually and skip failures
		result, err = parseFilesIndividually(files)
		if err != nil {
			return nil, fmt.Errorf("sqlc-schema-doc: failed to parse SQL: %w", err)
		}
	}

	// Build schema from AST
	tables, enums := buildSchema(result, opts.Exclude)

	// Generate markdown
	md := generateMarkdown(tables, enums)

	return &plugin.GenerateResponse{
		Files: []*plugin.File{
			{
				Name:     "SCHEMA.md",
				Contents: []byte(md),
			},
		},
	}, nil
}

func parseFilesIndividually(files []string) (*pg_query.ParseResult, error) {
	combined := &pg_query.ParseResult{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		result, err := pg_query.Parse(string(data))
		if err != nil {
			// Skip files that fail to parse (e.g., complex DO blocks)
			continue
		}
		combined.Stmts = append(combined.Stmts, result.Stmts...)
	}
	if len(combined.Stmts) == 0 {
		return nil, fmt.Errorf("no statements could be parsed")
	}
	return combined, nil
}

func buildSchema(result *pg_query.ParseResult, excludePrefixes []string) ([]table, []enum) {
	tableMap := make(map[string]*table)
	var tableOrder []string
	var enums []enum

	for _, rawStmt := range result.Stmts {
		stmt := rawStmt.Stmt

		switch {
		case stmt.GetCreateStmt() != nil:
			t := processCreateTable(stmt.GetCreateStmt())
			if t != nil && !shouldExclude(t.Name, excludePrefixes) {
				tableMap[t.Name] = t
				tableOrder = append(tableOrder, t.Name)
			}

		case stmt.GetCreateEnumStmt() != nil:
			e := processCreateEnum(stmt.GetCreateEnumStmt())
			if e != nil && !shouldExclude(e.Name, excludePrefixes) {
				enums = append(enums, *e)
			}

		case stmt.GetAlterTableStmt() != nil:
			processAlterTable(stmt.GetAlterTableStmt(), tableMap)
		}
	}

	// Build ordered table list
	tables := make([]table, 0, len(tableOrder))
	for _, name := range tableOrder {
		if t, ok := tableMap[name]; ok {
			tables = append(tables, *t)
		}
	}

	return tables, enums
}

func processCreateTable(stmt *pg_query.CreateStmt) *table {
	if stmt.Relation == nil {
		return nil
	}

	t := &table{
		Name: stmt.Relation.Relname,
	}

	for _, elt := range stmt.TableElts {
		colDef := elt.GetColumnDef()
		if colDef == nil {
			continue
		}

		col := extractColumn(colDef)
		t.Columns = append(t.Columns, col)
	}

	// Process table-level constraints
	for _, elt := range stmt.TableElts {
		constraint := elt.GetConstraint()
		if constraint == nil {
			continue
		}
		switch constraint.Contype {
		case pg_query.ConstrType_CONSTR_FOREIGN:
			applyForeignKey(t, constraint)
		case pg_query.ConstrType_CONSTR_PRIMARY:
			applyPrimaryKey(t, constraint)
		case pg_query.ConstrType_CONSTR_UNIQUE:
			applyUnique(t, constraint)
		}
	}

	return t
}

func processCreateEnum(stmt *pg_query.CreateEnumStmt) *enum {
	if len(stmt.TypeName) == 0 {
		return nil
	}

	nameNode := stmt.TypeName[len(stmt.TypeName)-1]
	name := nameNode.GetString_().GetSval()
	if name == "" {
		return nil
	}

	e := &enum{Name: name}
	for _, val := range stmt.Vals {
		if s := val.GetString_().GetSval(); s != "" {
			e.Values = append(e.Values, s)
		}
	}

	return e
}

func processAlterTable(stmt *pg_query.AlterTableStmt, tableMap map[string]*table) {
	if stmt.Relation == nil {
		return
	}

	tableName := stmt.Relation.Relname
	t, exists := tableMap[tableName]
	if !exists {
		return
	}

	for _, cmd := range stmt.Cmds {
		alterCmd := cmd.GetAlterTableCmd()
		if alterCmd == nil {
			continue
		}

		switch alterCmd.Subtype {
		case pg_query.AlterTableType_AT_AddColumn:
			colDef := alterCmd.Def.GetColumnDef()
			if colDef == nil {
				continue
			}

			col := extractColumn(colDef)

			duplicate := false
			for _, existing := range t.Columns {
				if existing.Name == col.Name {
					duplicate = true
					break
				}
			}
			if !duplicate {
				t.Columns = append(t.Columns, col)
			}

		case pg_query.AlterTableType_AT_SetNotNull:
			colName := alterCmd.Name
			for i, col := range t.Columns {
				if col.Name == colName {
					t.Columns[i].NotNull = true
					break
				}
			}

		case pg_query.AlterTableType_AT_DropNotNull:
			colName := alterCmd.Name
			for i, col := range t.Columns {
				if col.Name == colName {
					t.Columns[i].NotNull = false
					break
				}
			}

		case pg_query.AlterTableType_AT_AddConstraint:
			constraint := alterCmd.Def.GetConstraint()
			if constraint == nil {
				continue
			}
			switch constraint.Contype {
			case pg_query.ConstrType_CONSTR_FOREIGN:
				applyForeignKey(t, constraint)
			case pg_query.ConstrType_CONSTR_PRIMARY:
				applyPrimaryKey(t, constraint)
			case pg_query.ConstrType_CONSTR_UNIQUE:
				applyUnique(t, constraint)
			}
		}
	}
}

func extractColumn(colDef *pg_query.ColumnDef) column {
	col := column{
		Name: colDef.Colname,
	}

	if colDef.TypeName != nil {
		col.Type = extractTypeName(colDef.TypeName)
		col.IsArray = len(colDef.TypeName.ArrayBounds) > 0
		if col.IsArray {
			col.Type = col.Type + "[]"
		}
	}

	for _, c := range colDef.Constraints {
		constraint := c.GetConstraint()
		if constraint == nil {
			continue
		}
		switch constraint.Contype {
		case pg_query.ConstrType_CONSTR_NOTNULL:
			col.NotNull = true
		case pg_query.ConstrType_CONSTR_PRIMARY:
			col.NotNull = true
			col.PrimaryKey = true
		case pg_query.ConstrType_CONSTR_UNIQUE:
			col.Unique = true
		case pg_query.ConstrType_CONSTR_DEFAULT:
			col.Default = deparseExpr(constraint.RawExpr)
		case pg_query.ConstrType_CONSTR_FOREIGN:
			col.References = buildReference(constraint)
		}
	}

	return col
}

func extractTypeName(typeName *pg_query.TypeName) string {
	if typeName == nil || len(typeName.Names) == 0 {
		return "unknown"
	}

	last := typeName.Names[len(typeName.Names)-1]
	name := last.GetString_().GetSval()

	switch name {
	case "int4":
		return "integer"
	case "int8":
		return "bigint"
	case "int2":
		return "smallint"
	case "float4":
		return "real"
	case "float8":
		return "double precision"
	case "bool":
		return "boolean"
	case "varchar":
		return "varchar"
	}

	return name
}

func deparseExpr(node *pg_query.Node) string {
	if node == nil {
		return ""
	}
	selectStmt := &pg_query.Node{
		Node: &pg_query.Node_SelectStmt{
			SelectStmt: &pg_query.SelectStmt{
				TargetList: []*pg_query.Node{
					{
						Node: &pg_query.Node_ResTarget{
							ResTarget: &pg_query.ResTarget{
								Val: node,
							},
						},
					},
				},
			},
		},
	}
	result := &pg_query.ParseResult{
		Stmts: []*pg_query.RawStmt{{Stmt: selectStmt}},
	}
	deparsed, err := pg_query.Deparse(result)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(deparsed, "SELECT ")
}

func buildReference(constraint *pg_query.Constraint) string {
	if constraint.Pktable == nil {
		return ""
	}
	ref := constraint.Pktable.Relname
	if len(constraint.PkAttrs) > 0 {
		ref += "(" + constraint.PkAttrs[0].GetString_().GetSval() + ")"
	}
	return ref
}

func applyPrimaryKey(t *table, constraint *pg_query.Constraint) {
	for _, attr := range constraint.Keys {
		pkCol := attr.GetString_().GetSval()
		for i, c := range t.Columns {
			if c.Name == pkCol {
				t.Columns[i].PrimaryKey = true
				t.Columns[i].NotNull = true
				break
			}
		}
	}
}

func applyUnique(t *table, constraint *pg_query.Constraint) {
	for _, attr := range constraint.Keys {
		col := attr.GetString_().GetSval()
		for i, c := range t.Columns {
			if c.Name == col {
				t.Columns[i].Unique = true
				break
			}
		}
	}
}

func applyForeignKey(t *table, constraint *pg_query.Constraint) {
	if constraint.Pktable == nil || len(constraint.FkAttrs) == 0 {
		return
	}
	fkCol := constraint.FkAttrs[0].GetString_().GetSval()
	ref := buildReference(constraint)
	for i, c := range t.Columns {
		if c.Name == fkCol {
			t.Columns[i].References = ref
			break
		}
	}
}

func shouldExclude(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func generateMarkdown(tables []table, enums []enum) string {
	var sb strings.Builder

	sb.WriteString("# Database Schema\n\n")
	sb.WriteString("> Auto-generated by [sqlc-schema-doc](https://github.com/sonereker/sqlc-schema-doc). Do not edit.\n\n")

	if len(enums) > 0 {
		sb.WriteString("## Enums\n\n")
		for _, e := range enums {
			sb.WriteString(fmt.Sprintf("### %s\n", e.Name))
			quoted := make([]string, len(e.Values))
			for i, v := range e.Values {
				quoted[i] = "`" + v + "`"
			}
			sb.WriteString(strings.Join(quoted, " | "))
			sb.WriteString("\n\n")
		}
	}

	if len(tables) > 0 {
		sb.WriteString("## Tables\n\n")
		for _, t := range tables {
			sb.WriteString(fmt.Sprintf("### %s\n\n", t.Name))

			if len(t.Columns) == 0 {
				sb.WriteString("_No columns defined._\n\n")
				continue
			}

			sb.WriteString("| Column | Type | Nullable | PK | Unique | Default | References |\n")
			sb.WriteString("|--------|------|----------|----|--------|---------|------------|\n")
			for _, c := range t.Columns {
				nullable := "YES"
				if c.NotNull {
					nullable = "NO"
				}
				pk := ""
				if c.PrimaryKey {
					pk = "YES"
				}
				unique := ""
				if c.Unique {
					unique = "YES"
				}
				sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s |\n", c.Name, c.Type, nullable, pk, unique, c.Default, c.References))
			}
			sb.WriteString("\n")
		}
	}

	return sb.String()
}
