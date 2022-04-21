package postgres

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/k0kubun/sqldef/adapter"
	_ "github.com/lib/pq"
)

const indent = "    "

type PostgresDatabase struct {
	config adapter.Config
	db     *sql.DB
}

func NewDatabase(config adapter.Config) (adapter.Database, error) {
	db, err := sql.Open("postgres", postgresBuildDSN(config))
	if err != nil {
		return nil, err
	}

	return &PostgresDatabase{
		db:     db,
		config: config,
	}, nil
}

func (d *PostgresDatabase) TableNames() ([]string, error) {
	rows, err := d.db.Query(
		`select table_schema, table_name from information_schema.tables
		 where table_schema not in ('information_schema', 'pg_catalog')
		 and (table_schema != 'public' or table_name != 'pg_buffercache')
		 and table_type = 'BASE TABLE';`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tables := []string{}
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			return nil, err
		}
		tables = append(tables, schema+"."+name)
	}
	return tables, nil
}

var (
	suffixSemicolon = regexp.MustCompile(`;$`)
	spaces          = regexp.MustCompile(`[ ]+`)
)

func (d *PostgresDatabase) Views() ([]string, error) {
	rows, err := d.db.Query(
		`select table_schema, table_name, definition from information_schema.tables
		 inner join pg_views on table_name = viewname
		 where table_schema not in ('information_schema', 'pg_catalog', 'repack')
		 and (table_schema != 'public' or table_name != 'pg_buffercache')
		 and table_type = 'VIEW';`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ddls []string
	for rows.Next() {
		var schema, name, definition string
		if err := rows.Scan(&schema, &name, &definition); err != nil {
			return nil, err
		}
		definition = strings.TrimSpace(definition)
		definition = strings.ReplaceAll(definition, "\n", "")
		definition = suffixSemicolon.ReplaceAllString(definition, "")
		definition = spaces.ReplaceAllString(definition, " ")
		ddls = append(
			ddls, fmt.Sprintf(
				"CREATE VIEW %s AS %s;", schema+"."+name, definition,
			),
		)
	}
	return ddls, nil
}

func (d *PostgresDatabase) Triggers() ([]string, error) {
	return nil, nil
}

func (d *PostgresDatabase) Types() ([]string, error) {
	rows, err := d.db.Query(
		`select t.typname, string_agg(e.enumlabel, ' ')
		 from pg_enum e
		 join pg_type t on e.enumtypid = t.oid
		 group by t.typname;`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ddls []string
	for rows.Next() {
		var typeName, labels string
		if err := rows.Scan(&typeName, &labels); err != nil {
			return nil, err
		}
		enumLabels := []string{}
		for _, label := range strings.Split(labels, " ") {
			enumLabels = append(enumLabels, fmt.Sprintf("'%s'", label))
		}
		ddls = append(
			ddls, fmt.Sprintf(
				"CREATE TYPE %s AS ENUM (%s);", typeName, strings.Join(enumLabels, ", "),
			),
		)
	}
	return ddls, nil
}

func (d *PostgresDatabase) DumpTableDDL(table string) (string, error) {
	cols, err := d.getColumns(table)
	if err != nil {
		return "", err
	}
	pkeyCols, err := d.getPrimaryKeyColumns(table)
	if err != nil {
		return "", err
	}
	indexDefs, err := d.getIndexDefs(table)
	if err != nil {
		return "", err
	}
	foreignDefs, err := d.getForeignDefs(table)
	if err != nil {
		return "", err
	}
	policyDefs, err := d.getPolicyDefs(table)
	if err != nil {
		return "", err
	}
	checkConstraints, err := d.getTableCheckConstraints(table)
	if err != nil {
		return "", err
	}
	uniqueConstraints, err := d.getUniqueConstraints(table)
	if err != nil {
		return "", err
	}
	return buildDumpTableDDL(table, cols, pkeyCols, indexDefs, foreignDefs, policyDefs, checkConstraints, uniqueConstraints), nil
}

func buildDumpTableDDL(table string, columns []column, pkeyCols, indexDefs, foreignDefs, policyDefs []string, checkConstraints, uniqueConstraints map[string]string) string {
	var queryBuilder strings.Builder
	fmt.Fprintf(&queryBuilder, "CREATE TABLE %s (", table)
	for i, col := range columns {
		if i > 0 {
			fmt.Fprint(&queryBuilder, ",")
		}
		fmt.Fprint(&queryBuilder, "\n"+indent)
		fmt.Fprintf(&queryBuilder, "\"%s\" %s", col.Name, col.GetDataType())
		if col.Length > 0 {
			fmt.Fprintf(&queryBuilder, "(%d)", col.Length)
		}
		if !col.Nullable {
			fmt.Fprint(&queryBuilder, " NOT NULL")
		}
		if col.Default != "" && !col.IsAutoIncrement {
			fmt.Fprintf(&queryBuilder, " DEFAULT %s", col.Default)
		}
		if col.IdentityGeneration != "" {
			fmt.Fprintf(&queryBuilder, " GENERATED %s AS IDENTITY", col.IdentityGeneration)
		}
		if col.Check != nil {
			fmt.Fprintf(&queryBuilder, " CONSTRAINT %s %s", col.Check.name, col.Check.definition)
		}
	}
	if len(pkeyCols) > 0 {
		fmt.Fprint(&queryBuilder, ",\n"+indent)
		fmt.Fprintf(&queryBuilder, "PRIMARY KEY (\"%s\")", strings.Join(pkeyCols, "\", \""))
	}
	for constraintName, constraintDef := range checkConstraints {
		fmt.Fprint(&queryBuilder, ",\n"+indent)
		fmt.Fprintf(&queryBuilder, "CONSTRAINT %s %s", constraintName, constraintDef)
	}
	fmt.Fprintf(&queryBuilder, "\n);\n")
	for _, v := range indexDefs {
		fmt.Fprintf(&queryBuilder, "%s;\n", v)
	}
	for _, v := range foreignDefs {
		fmt.Fprintf(&queryBuilder, "%s;\n", v)
	}
	for _, v := range policyDefs {
		fmt.Fprintf(&queryBuilder, "%s;\n", v)
	}
	for _, constraintDef := range uniqueConstraints {
		fmt.Fprintf(&queryBuilder, "%s;\n", constraintDef)
	}
	return strings.TrimSuffix(queryBuilder.String(), "\n")
}

type columnConstraint struct {
	definition string
	name       string
}

type column struct {
	Name               string
	dataType           string
	Length             int
	Nullable           bool
	Default            string
	IsAutoIncrement    bool
	IdentityGeneration string
	Check              *columnConstraint
}

func (c *column) GetDataType() string {
	switch c.dataType {
	case "smallint":
		if c.IsAutoIncrement {
			return "smallserial"
		}
		return c.dataType
	case "integer":
		if c.IsAutoIncrement {
			return "serial"
		}
		return c.dataType
	case "bigint":
		if c.IsAutoIncrement {
			return "bigserial"
		}
		return c.dataType
	case "timestamp without time zone":
		// Note:
		// The SQL standard requires that writing just timestamp be equivalent to timestamp without time zone, and PostgreSQL honors that behavior.
		// timestamptz is accepted as an abbreviation for timestamp with time zone; this is a PostgreSQL extension.
		// https://www.postgresql.org/docs/9.6/datatype-datetime.html
		return "timestamp"
	case "time without time zone":
		return "time"
	default:
		return c.dataType
	}
}

func (d *PostgresDatabase) getColumns(table string) ([]column, error) {
	const query = `WITH
	  columns AS (
	    SELECT
	      s.column_name,
	      s.column_default,
	      s.is_nullable,
	      s.character_maximum_length,
	      CASE
	      WHEN s.data_type IN ('ARRAY', 'USER-DEFINED') THEN format_type(f.atttypid, f.atttypmod)
	      ELSE s.data_type
	      END,
	      s.identity_generation
	    FROM pg_attribute f
	    JOIN pg_class c ON c.oid = f.attrelid JOIN pg_type t ON t.oid = f.atttypid
	    LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = f.attnum
	    LEFT JOIN pg_namespace n ON n.oid = c.relnamespace
	    LEFT JOIN information_schema.columns s ON s.column_name = f.attname AND s.table_name = c.relname AND s.table_schema = n.nspname
	    WHERE c.relkind = 'r'::char
	    AND n.nspname = $1
	    AND c.relname = $2
	    AND f.attnum > 0
	    ORDER BY f.attnum
	  ),
	  column_constraints AS (
	    SELECT att.attname column_name, tmp.name, tmp.type , tmp.definition
	    FROM (
	      SELECT unnest(con.conkey) AS conkey,
	             pg_get_constraintdef(con.oid, true) AS definition,
	             cls.oid AS relid,
	             con.conname AS name,
	             con.contype AS type
	      FROM   pg_constraint con
	      JOIN   pg_namespace nsp ON nsp.oid = con.connamespace
	      JOIN   pg_class cls ON cls.oid = con.conrelid
	      WHERE  nsp.nspname = $1
	      AND    cls.relname = $2
	      AND    array_length(con.conkey, 1) = 1
	    ) tmp
	    JOIN pg_attribute att ON tmp.conkey = att.attnum AND tmp.relid = att.attrelid
	  ),
	  check_constraints AS (
	    SELECT column_name, name, definition
	    FROM   column_constraints
	    WHERE  type = 'c'
	  )
	SELECT    columns.*, checks.name, checks.definition
	FROM      columns
	LEFT JOIN check_constraints checks USING (column_name);`

	schema, table := SplitTableName(table)
	rows, err := d.db.Query(query, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols := make([]column, 0)
	for rows.Next() {
		col := column{}
		var colName, isNullable, dataType string
		var maxLenStr, colDefault, idGen, checkName, checkDefinition *string
		err = rows.Scan(&colName, &colDefault, &isNullable, &maxLenStr, &dataType, &idGen, &checkName, &checkDefinition)
		if err != nil {
			return nil, err
		}
		var maxLen int
		if maxLenStr != nil {
			maxLen, err = strconv.Atoi(*maxLenStr)
			if err != nil {
				return nil, err
			}
		}
		col.Name = strings.Trim(colName, `" `)
		if colDefault != nil {
			col.Default = *colDefault
		}
		if colDefault != nil && strings.HasPrefix(*colDefault, "nextval(") {
			col.IsAutoIncrement = true
		}
		col.Nullable = isNullable == "YES"
		col.dataType = dataType
		col.Length = maxLen
		if idGen != nil {
			col.IdentityGeneration = *idGen
		}
		if checkName != nil && checkDefinition != nil {
			col.Check = &columnConstraint{
				definition: *checkDefinition,
				name:       *checkName,
			}
		}
		cols = append(cols, col)
	}
	return cols, nil
}

func (d *PostgresDatabase) getIndexDefs(table string) ([]string, error) {
	// Exclude indexes that are implicitly created for primary keys or unique constraints.
	const query = `WITH
	  unique_and_pk_constraints AS (
	    SELECT con.conname AS name
	    FROM   pg_constraint con
	    JOIN   pg_namespace nsp ON nsp.oid = con.connamespace
	    JOIN   pg_class cls ON cls.oid = con.conrelid
	    WHERE  con.contype IN ('p', 'u')
	    AND    nsp.nspname = $1
	    AND    cls.relname = $2
	  )
	SELECT indexName, indexdef
	FROM   pg_indexes
	WHERE  schemaname = $1
	AND    tablename = $2
	AND    indexName NOT IN (SELECT name FROM unique_and_pk_constraints)
	`
	schema, table := SplitTableName(table)
	rows, err := d.db.Query(query, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	indexes := make([]string, 0)
	for rows.Next() {
		var indexName, indexdef string
		err = rows.Scan(&indexName, &indexdef)
		if err != nil {
			return nil, err
		}
		indexName = strings.Trim(indexName, `" `)

		indexes = append(indexes, indexdef)
	}
	return indexes, nil
}

func (d *PostgresDatabase) getTableCheckConstraints(tableName string) (map[string]string, error) {
	const query = `SELECT con.conname, pg_get_constraintdef(con.oid, true)
	FROM   pg_constraint con
	JOIN   pg_namespace nsp ON nsp.oid = con.connamespace
	JOIN   pg_class cls ON cls.oid = con.conrelid
	WHERE  con.contype = 'c'
	AND    nsp.nspname = $1
	AND    cls.relname = $2
	AND    array_length(con.conkey, 1) > 1;`

	result := map[string]string{}
	schema, table := SplitTableName(tableName)
	rows, err := d.db.Query(query, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var constraintName, constraintDef string
		err = rows.Scan(&constraintName, &constraintDef)
		if err != nil {
			return nil, err
		}
		result[constraintName] = constraintDef
	}

	return result, nil
}

func (d *PostgresDatabase) getUniqueConstraints(tableName string) (map[string]string, error) {
	const query = `SELECT con.conname, pg_get_constraintdef(con.oid)
	FROM   pg_constraint con
	JOIN   pg_namespace nsp ON nsp.oid = con.connamespace
	JOIN   pg_class cls ON cls.oid = con.conrelid
	WHERE  con.contype = 'u'
	AND    nsp.nspname = $1
	AND    cls.relname = $2;`

	result := map[string]string{}
	schema, table := SplitTableName(tableName)
	rows, err := d.db.Query(query, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var constraintName, constraintDef string
		err = rows.Scan(&constraintName, &constraintDef)
		if err != nil {
			return nil, err
		}
		result[constraintName] = fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s", tableName, constraintName, constraintDef)
	}

	return result, nil
}

func (d *PostgresDatabase) getPrimaryKeyColumns(table string) ([]string, error) {
	const query = `SELECT
	tc.table_schema, tc.constraint_name, tc.table_name, kcu.column_name
FROM
	information_schema.table_constraints AS tc
	JOIN information_schema.key_column_usage AS kcu
		USING (table_schema, table_name, constraint_name)
WHERE constraint_type = 'PRIMARY KEY' AND tc.table_schema=$1 AND tc.table_name=$2 ORDER BY kcu.ordinal_position`
	schema, table := SplitTableName(table)
	rows, err := d.db.Query(query, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columnNames := make([]string, 0)
	var tableSchema, constraintName, tableName string
	for rows.Next() {
		var columnName string
		err = rows.Scan(&tableSchema, &constraintName, &tableName, &columnName)
		if err != nil {
			return nil, err
		}
		columnNames = append(columnNames, columnName)
	}
	return columnNames, nil
}

// refs: https://gist.github.com/PickledDragon/dd41f4e72b428175354d
func (d *PostgresDatabase) getForeignDefs(table string) ([]string, error) {
	const query = `SELECT
	tc.table_schema, tc.constraint_name, tc.table_name, kcu.column_name,
	ccu.table_schema AS foreign_table_schema,
	ccu.table_name AS foreign_table_name,
	ccu.column_name AS foreign_column_name,
	rc.update_rule AS foreign_update_rule,
	rc.delete_rule AS foreign_delete_rule
FROM
	information_schema.table_constraints AS tc
	JOIN information_schema.key_column_usage AS kcu
		ON tc.constraint_name = kcu.constraint_name
	JOIN information_schema.constraint_column_usage AS ccu
		ON tc.constraint_name = ccu.constraint_name
	JOIN information_schema.referential_constraints AS rc
		ON tc.constraint_name = rc.constraint_name
WHERE constraint_type = 'FOREIGN KEY' AND tc.table_schema=$1 AND tc.table_name=$2`
	schema, table := SplitTableName(table)
	rows, err := d.db.Query(query, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	defs := make([]string, 0)
	for rows.Next() {
		var tableSchema, constraintName, tableName, columnName, foreignTableSchema, foreignTableName, foreignColumnName, foreignUpdateRule, foreignDeleteRule string
		err = rows.Scan(&tableSchema, &constraintName, &tableName, &columnName, &foreignTableSchema, &foreignTableName, &foreignColumnName, &foreignUpdateRule, &foreignDeleteRule)
		if err != nil {
			return nil, err
		}
		def := fmt.Sprintf(
			"ALTER TABLE ONLY %s.%s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s.%s(%s) ON UPDATE %s ON DELETE %s",
			tableSchema, tableName, constraintName, columnName, foreignTableSchema, foreignTableName, foreignColumnName, foreignUpdateRule, foreignDeleteRule,
		)
		defs = append(defs, def)
	}
	return defs, nil
}

var (
	policyRolesPrefixRegex = regexp.MustCompile(`^{`)
	policyRolesSuffixRegex = regexp.MustCompile(`}$`)
)

func (d *PostgresDatabase) getPolicyDefs(table string) ([]string, error) {
	version, err := d.Version()
	if err != nil {
		return nil, err
	}

	const queryPermissive = "SELECT policyname, permissive, roles, cmd, qual, with_check FROM pg_policies WHERE schemaname = $1 AND tablename = $2;"
	const queryNone = "SELECT policyname, '', roles, cmd, qual, with_check FROM pg_policies WHERE schemaname = $1 AND tablename = $2;"

	var query string
	var r9 = regexp.MustCompile(`^9`)
	if r9.MatchString(version) {
		query = queryNone
	} else {
		query = queryPermissive
	}
	schema, table := SplitTableName(table)
	rows, err := d.db.Query(query, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	defs := make([]string, 0)
	for rows.Next() {
		var (
			policyName, permissive, roles, cmd string
			using, withCheck                   sql.NullString
		)
		err = rows.Scan(&policyName, &permissive, &roles, &cmd, &using, &withCheck)
		if err != nil {
			return nil, err
		}
		roles = policyRolesPrefixRegex.ReplaceAllString(roles, "")
		roles = policyRolesSuffixRegex.ReplaceAllString(roles, "")
		def := fmt.Sprintf(
			"CREATE POLICY %s ON %s AS %s FOR %s TO %s",
			policyName, table, permissive, cmd, roles,
		)
		if using.Valid {
			def += fmt.Sprintf(" USING %s", using.String)
		}
		if withCheck.Valid {
			def += fmt.Sprintf(" WITH CHECK %s", withCheck.String)
		}
		defs = append(defs, def+";")
	}
	return defs, nil
}

func (d *PostgresDatabase) Version() (string, error) {
	rows, err := d.db.Query("SELECT name, setting, min_val, max_val FROM pg_settings WHERE name = 'server_version_num'")

	// ex) on PostgreSQL 9.6.24
	// | name               | setting | min_val | max_val |
	// | ------------------ | ------- | ------- | ------- |
	// | server_version_num | 90624   | 90624   | 90624   |

	if err != nil {
		return "", err
	}
	defer rows.Close()

	var version string
	for rows.Next() {
		var name, setting, min_val, max_val string
		err = rows.Scan(&name, &setting, &min_val, &max_val)
		if err != nil {
			return "", err
		}
		version = setting
	}
	return version, nil
}

func (d *PostgresDatabase) DB() *sql.DB {
	return d.db
}

func (d *PostgresDatabase) Close() error {
	return d.db.Close()
}

func postgresBuildDSN(config adapter.Config) string {
	user := config.User
	password := config.Password
	database := config.DbName
	host := ""
	if config.Socket == "" {
		host = fmt.Sprintf("%s:%d", config.Host, config.Port)
	} else {
		host = config.Socket
	}

	var options []string
	if sslmode, ok := os.LookupEnv("PGSSLMODE"); ok { // TODO: have this in adapter.Config, or standardize config with DSN?
		options = append(options, fmt.Sprintf("sslmode=%s", sslmode)) // TODO: uri escape
	}

	if sslrootcert, ok := os.LookupEnv("PGSSLROOTCERT"); ok { // TODO: have this in adapter.Config, or standardize config with DSN?
		options = append(options, fmt.Sprintf("sslrootcert=%s", sslrootcert))
	}

	// `QueryEscape` instead of `PathEscape` so that colon can be escaped.
	return fmt.Sprintf("postgres://%s:%s@%s/%s?%s", url.QueryEscape(user), url.QueryEscape(password), host, database, strings.Join(options, "&"))
}

func SplitTableName(table string) (string, string) {
	schema := "public"
	schemaTable := strings.SplitN(table, ".", 2)
	if len(schemaTable) == 2 {
		schema = schemaTable[0]
		table = schemaTable[1]
	}
	return schema, table
}
