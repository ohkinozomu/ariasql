// Package catalog
// AriaSQL system catalog package
// Copyright (C) AriaSQL
// Author(s): Alex Gaetano Padula
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.
package catalog

import (
	"ariasql/shared"
	"ariasql/storage/btree"
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/DataDog/zstd"
	"github.com/google/uuid"
	"golang.org/x/crypto/chacha20"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const MAX_COLUMN_NAME_SIZE = 64 // Max 64 bytes for column name
const MAX_TABLE_NAME_SIZE = 64  // Max 64 bytes for table name
const MAX_INDEX_NAME_SIZE = 64  // Max 64 bytes for index name

// DB_SCHEMA_TABLE_SCHEMA_FILE_EXTENSION Table schema file extension
// The table schema file is used to store the schema of the table
const DB_SCHEMA_TABLE_SCHEMA_FILE_EXTENSION = ".schma" // Table schema file extension

// DB_SCHEMA_TABLE_DATA_FILE_EXTENSION Table data file extension
// The table data file is used to store the actual data of the table
const DB_SCHEMA_TABLE_DATA_FILE_EXTENSION = ".dat" // Table data

// DB_SCHEMA_TABLE_INDEX_FILE_EXTENSION Index file extension
// The index file is used to store the index data
const DB_SCHEMA_TABLE_INDEX_FILE_EXTENSION = ".idx" // Index file extension

const SYS_USERS_EXTENSION = ".usrs" // Users file extension

const DB_PROC_EXTENSION = ".proc" // Procedure file extension

// DB_SCHEMA_TABLE_SEQ_FILE_EXTENSION Table count file extension
// The table count file is used to store the number of rows in a table
// Used for sequence columns (there can only be one sequence column per table)
// The sequence column is a column that auto increments based on the number of rows in the table
const DB_SCHEMA_TABLE_SEQ_FILE_EXTENSION = ".seq" // Table seq file extension

// Catalog is the root of the database catalog
type Catalog struct {
	Databases     map[string]*Database // Databases is a map of database names to database objects
	Directory     string               // Directory is the directory where database catalog data is stored
	Users         map[string]*User     // Users is a map of user names to user objects
	UsersFile     *os.File             // Users file
	UsersFileLock *sync.Mutex          // Users file lock
	UsersLock     *sync.Mutex          // Users lock
	DatabasesLock *sync.Mutex          // Databases lock
}

// Database is a database object
type Database struct {
	Name               string                // Name is the database name
	Tables             map[string]*Table     // Tables within database
	TablesLock         *sync.Mutex           // Tables slice mutex
	Directory          string                // Directory is the directory where database data is stored
	Procedures         map[string]*Procedure // Procedures is a map of procedure names to procedure objects
	ProceduresFile     *os.File              // Procedures file
	ProceduresFileLock *sync.Mutex           // Procedures lock
}

// Table is a table object
type Table struct {
	Name         string            // Name is the table name
	Indexes      map[string]*Index // Indexes is a map of index names to index objects
	Rows         *btree.Pager      // Rows is the btree pager for the table.  We use the pager to page our table data
	TableSchema  *TableSchema      // TableSchema is the schema of the table
	Directory    string            // Directory is the directory where table data is stored
	SequenceFile *os.File          // Table sequence file
	SeqLock      *sync.Mutex       // Sequence mutex
	Compress     bool              // Compress is true if the table data is compressed
	Encrypt      bool              // Encrypt is true if the table data is encrypted
	HashedKey    [32]byte          // HashedKey is the hashed key used to encrypt the table data
	Nonce        [12]byte          // Nonce is the nonce used to encrypt the table data
}

// Procedure is a procedure object
type Procedure struct {
	Name string      // Name is the procedure name
	Proc interface{} // *parser.Procedure
}

// TableSchema is the schema of a table
type TableSchema struct {
	ColumnDefinitions map[string]*ColumnDefinition // ColumnDefinitions is a map of column names to column definitions
}

// ColumnDefinition is a column definition
type ColumnDefinition struct {
	DataType   string      // Column data type
	NotNull    bool        // Column cannot be null
	Sequence   bool        // Column is auto increment/sequence
	Unique     bool        // Column is unique
	Length     int         // Column length
	Scale      int         // Column scale
	Precision  int         // Column precision
	References *Reference  // References is a foreign key reference
	Default    interface{} // Default value for the column
	Check      interface{} // Check constraint for the column
}

// Reference is a reference to another table
// Is a foreign key reference
type Reference struct {
	TableName  string // Table name
	ColumnName string // Column name
}

// Index is an index object
type Index struct {
	Name    string       // Name is the index name
	Columns []string     // Columns is a list of column names in the index
	Unique  bool         // Unique is true if the index is unique, there can only be one row with the same value
	btree   *btree.BTree // BTree is the Btree object for the index
	lock    *sync.Mutex  // Lock is the lock for the index
}

// User is a user object
type User struct {
	Username   string
	Password   string
	Privileges []*Privilege
}

// Privilege is a user privilege
type Privilege struct {
	DatabaseName     string // name or *
	TableName        string // name or *
	PrivilegeActions []shared.PrivilegeAction
}

// New creates a new catalog
func New(directory string) *Catalog {
	return &Catalog{
		Directory: directory,
	}
}

// Open initializes the catalog, reading all databases, tables, indexes, etc from disk
func (cat *Catalog) Open() error {
	gob.Register(&TableSchema{})
	gob.Register(&shared.SysDate{})
	gob.Register(&shared.SysTime{})
	gob.Register(&shared.SysTimestamp{})
	gob.Register(&shared.GenUUID{})
	gob.Register(time.Time{})

	cat.Databases = make(map[string]*Database)

	// Check for databases directory
	_, err := os.Stat(fmt.Sprintf("%s%sdatabases", cat.Directory, shared.GetOsPathSeparator()))
	if os.IsNotExist(err) {
		// Create databases directory
		err = os.MkdirAll(fmt.Sprintf("%s%sdatabases", cat.Directory, shared.GetOsPathSeparator()), 0755)
		if err != nil {
			return err
		}

	} else {
		// Read databases
		databaseDirs, err := os.ReadDir(fmt.Sprintf("%s%sdatabases", cat.Directory, shared.GetOsPathSeparator()))
		if err != nil {
			return err
		}

		for _, databaseDir := range databaseDirs {
			if databaseDir.IsDir() {
				db := &Database{
					Directory: fmt.Sprintf("%s%sdatabases%s%s", cat.Directory, shared.GetOsPathSeparator(), shared.GetOsPathSeparator(), databaseDir.Name()),
				}

				db.TablesLock = &sync.Mutex{}
				db.Name = databaseDir.Name()
				cat.Databases[databaseDir.Name()] = db

				// Create procedures map
				db.Procedures = make(map[string]*Procedure)
				db.ProceduresFileLock = &sync.Mutex{}

				// Check if {db.name}.DB_PROC_EXTENSION exists
				if _, err := os.Stat(fmt.Sprintf("%s%s%s%s", db.Directory, shared.GetOsPathSeparator(), db.Name, DB_PROC_EXTENSION)); err == nil {
					// Open procedure file
					db.ProceduresFile, err = os.Open(fmt.Sprintf("%s%s%s%s", db.Directory, shared.GetOsPathSeparator(), db.Name, DB_PROC_EXTENSION))
					if err != nil {
						return err
					}

					// Decode procedures
					dec := gob.NewDecoder(db.ProceduresFile)
					err = dec.Decode(&db.Procedures)
					if err != nil {
						return err
					}

				}

				// Within databases directory there are table directories
				tblDirs, err := os.ReadDir(fmt.Sprintf("%s", db.Directory))
				if err != nil {
					return err
				}

				db.Tables = make(map[string]*Table)

				for _, tblDir := range tblDirs {
					if tblDir.IsDir() {
						tbl := &Table{
							Name:      tblDir.Name(),
							Directory: fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), tblDir.Name()),
						}

						// Within each table there is a schema file, index files , sequence file, and data file

						// Read schema file
						schemaFile, err := os.Open(fmt.Sprintf("%s%s%s", tbl.Directory, shared.GetOsPathSeparator(), fmt.Sprintf("%s%s", tblDir.Name(), DB_SCHEMA_TABLE_SCHEMA_FILE_EXTENSION)))
						if err != nil {
							return err
						}

						// Decode schema
						dec := gob.NewDecoder(schemaFile)
						tblSchema := &TableSchema{}
						err = dec.Decode(tblSchema)

						if err != nil {
							return err
						}

						tbl.TableSchema = tblSchema

						// Read data file
						rowFile, err := btree.OpenPager(fmt.Sprintf("%s%s%s", tbl.Directory, shared.GetOsPathSeparator(), fmt.Sprintf("%s%s", tblDir.Name(), DB_SCHEMA_TABLE_DATA_FILE_EXTENSION)), os.O_RDWR, 0755)
						if err != nil {
							return err
						}

						tbl.Rows = rowFile

						// Read sequence file
						seqFile, err := os.Open(fmt.Sprintf("%s%s%s", tbl.Directory, shared.GetOsPathSeparator(), fmt.Sprintf("%s%s", tblDir.Name(), DB_SCHEMA_TABLE_SEQ_FILE_EXTENSION)))
						if err != nil {
							return err
						}

						tbl.SequenceFile = seqFile

						tblFiles, err := os.ReadDir(fmt.Sprintf("%s", tbl.Directory))
						if err != nil {
							return err
						}

						tbl.Indexes = make(map[string]*Index)

						for _, tblFile := range tblFiles {
							if strings.HasSuffix(tblFile.Name(), DB_SCHEMA_TABLE_INDEX_FILE_EXTENSION) {
								// Read index file
								indexFile, err := os.Open(fmt.Sprintf("%s%s%s", tbl.Directory, shared.GetOsPathSeparator(), tblFile.Name()))
								if err != nil {
									return err
								}

								// Decode index
								dec := gob.NewDecoder(indexFile)
								idx := &Index{}
								err = dec.Decode(idx)

								if err != nil {
									return err
								}

								// Open btree
								bt, err := btree.Open(fmt.Sprintf("%s%s%s%s", tbl.Directory, shared.GetOsPathSeparator(), fmt.Sprintf("idx_%s", idx.Name), ".bt"), os.O_RDWR, 0755, 6)
								if err != nil {
									return err
								}

								idx.btree = bt

								tbl.Indexes[idx.Name] = idx
								tbl.Indexes[idx.Name].lock = &sync.Mutex{}

							}

						}

						db.Tables[tbl.Name] = tbl
					}
				}

			}
		}

	}

	// Open users file
	cat.Users = make(map[string]*User)

	cat.UsersFile, err = os.OpenFile(fmt.Sprintf("%s%susers%s", cat.Directory, shared.GetOsPathSeparator(), SYS_USERS_EXTENSION), os.O_CREATE|os.O_RDWR, 0755)
	if err != nil {
		return err

	}

	cat.UsersLock = &sync.Mutex{}
	cat.UsersFileLock = &sync.Mutex{}
	cat.DatabasesLock = &sync.Mutex{}

	err = cat.ReadUsersFromFile()
	if err != nil {

		if strings.Contains(err.Error(), "users file is empty") {
			// Create default user
			err = cat.CreateNewUser("admin", "admin")
			if err != nil {
				return err
			}

			err = cat.GrantPrivilegeToUser("admin", &Privilege{
				DatabaseName:     "*",
				TableName:        "*",
				PrivilegeActions: []shared.PrivilegeAction{shared.PRIV_ALL},
			})
			if err != nil {
				return err
			}

		}
		return err
	}

	return nil
}

// Close closes the catalog
func (cat *Catalog) Close() {
	for _, db := range cat.Databases {
		db.ProceduresFile.Close()

		for _, tbl := range db.Tables {
			if tbl.Rows != nil {
				tbl.Rows.Close()
			}
			for _, idx := range tbl.Indexes {
				if idx.btree != nil {
					idx.btree.Close()
				}
			}
		}
	}

}

// CreateDatabase create a new database
func (cat *Catalog) CreateDatabase(name string) error {
	// Check if database exists
	if _, ok := cat.Databases[name]; ok {
		return fmt.Errorf("database %s already exists", name)
	}

	// Create database directory
	err := os.Mkdir(fmt.Sprintf("%s%sdatabases%s%s", cat.Directory, shared.GetOsPathSeparator(), shared.GetOsPathSeparator(), name), 0755)
	if err != nil {
		return err
	}

	// Create database
	cat.Databases[name] = &Database{
		Name:               name,
		Tables:             make(map[string]*Table),
		Procedures:         make(map[string]*Procedure),
		ProceduresFileLock: &sync.Mutex{},
		Directory:          fmt.Sprintf("%s%sdatabases%s%s", cat.Directory, shared.GetOsPathSeparator(), shared.GetOsPathSeparator(), name),
	}

	// Create procedures file
	procFile, err := os.Create(fmt.Sprintf("%s%s%s%s", cat.Databases[name].Directory, shared.GetOsPathSeparator(), name, DB_PROC_EXTENSION))
	if err != nil {
		return err
	}

	cat.Databases[name].ProceduresFile = procFile

	cat.Databases[name].ProceduresFileLock.Lock()
	defer cat.Databases[name].ProceduresFileLock.Unlock()

	// Write to procedures file
	enc := gob.NewEncoder(procFile)
	err = enc.Encode(cat.Databases[name].Procedures)
	if err != nil {
		return err

	}

	return nil
}

// DropDatabase drops a database by name
func (cat *Catalog) DropDatabase(name string) error {
	// Check if database exists
	if _, ok := cat.Databases[name]; !ok {
		return fmt.Errorf("database %s does not exist", name)
	}

	// Drop database directory
	err := os.RemoveAll(cat.Databases[name].Directory)
	if err != nil {
		return err
	}

	// Drop database
	delete(cat.Databases, name)

	return nil
}

// GetIndexes gets the indexes for a table
func (tbl *Table) GetIndexes() []*Index {
	indexes := make([]*Index, 0)

	for _, idx := range tbl.Indexes {
		indexes = append(indexes, idx)

	}

	return indexes

}

// GetLock get btree lock
func (idx *Index) GetLock() *sync.Mutex {
	return idx.lock
}

// DropTable drops a table by name
func (db *Database) DropTable(name string) error {
	// Check if table exists
	if _, ok := db.Tables[name]; !ok {
		return fmt.Errorf("table %s does not exist", name)
	}

	// Drop table
	delete(db.Tables, name)

	// Drop table directory
	err := os.RemoveAll(fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), name))
	if err != nil {
		return err
	}

	return nil

}

// CreateTable creates a new table in a schema
func (db *Database) CreateTable(name string, tblSchema *TableSchema, encrypt bool, compress bool, key []byte) error {
	if tblSchema == nil {
		return fmt.Errorf("table schema is nil")
	}

	if len(name) > MAX_TABLE_NAME_SIZE {
		return fmt.Errorf("table name is too long, max length is %d", MAX_TABLE_NAME_SIZE)
	}

	// Check if table exists
	if _, ok := db.Tables[name]; ok {
		return fmt.Errorf("table %s already exists", name)
	}

	// Create table
	db.Tables[name] = &Table{
		Name:        name,
		Indexes:     make(map[string]*Index),
		TableSchema: tblSchema,
		Directory:   fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), name),
	}

	// Create table directory
	err := os.Mkdir(fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), name), 0755)
	if err != nil {
		return err
	}

	sequenceDefined := false

	for colName, colDef := range tblSchema.ColumnDefinitions {
		if len(colName) > MAX_COLUMN_NAME_SIZE {
			// delete table
			delete(db.Tables, name)
			os.RemoveAll(fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), name))
			return fmt.Errorf("column name is too long, max length is %d", MAX_COLUMN_NAME_SIZE)
		}

		if !shared.IsValidDataType(colDef.DataType) {
			delete(db.Tables, name)
			os.RemoveAll(fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), name))
			return fmt.Errorf("invalid data type %s", colDef.DataType)
		}

		if colDef.Unique {
			err = db.Tables[name].CreateIndex(fmt.Sprintf("unique_%s", colName), []string{colName}, true)
			if err != nil {
				delete(db.Tables, name)
				os.RemoveAll(fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), name))
				return err
			}
		}

		if colDef.Sequence {
			if sequenceDefined {
				delete(db.Tables, name)
				os.RemoveAll(fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), name))
				return fmt.Errorf("only one sequence column is allowed per table")
			}

			// Sequenced column must be unique and not null

			if !colDef.Unique || !colDef.NotNull {
				delete(db.Tables, name)
				os.RemoveAll(fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), name))
				return fmt.Errorf("sequence column %s must be unique and not null", colName)
			}

			// Datatype MUST be an integer
			if strings.ToUpper(colDef.DataType) != "INT" && strings.ToUpper(colDef.DataType) != "INTEGER" {
				delete(db.Tables, name)
				os.RemoveAll(fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), name))
				return fmt.Errorf("sequence column %s must be an integer", colName)
			}

			sequenceDefined = true
		}

		switch strings.ToUpper(colDef.DataType) {
		case "CHARACTER", "CHAR":
			// A character datatype requires a length
			if colDef.Length == 0 {
				delete(db.Tables, name)
				os.RemoveAll(fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), name))
				return fmt.Errorf("column %s requires a length", colName)
			}
		case "NUMERIC", "DECIMAL", "DEC", "FLOAT", "DOUBLE", "REAL":
			// A numeric datatype requires a precision and scale
			if colDef.Precision == 0 {
				delete(db.Tables, name)
				os.RemoveAll(fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), name))
				return fmt.Errorf("column %s requires a precision", colName)
			}

			if colDef.Scale == 0 {
				delete(db.Tables, name)
				os.RemoveAll(fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), name))
				return fmt.Errorf("column %s requires a scale", colName)
			}
		case "INT", "INTEGER", "SMALLINT":
		case "DATE", "TIME", "TIMESTAMP", "DATETIME":
		case "BINARY":
		case "UUID":
		case "BOOLEAN", "BOOL":
		case "TEXT":
		case "BLOB":

		default:
			delete(db.Tables, name)
			os.RemoveAll(fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), name))
			return fmt.Errorf("invalid data type %s", colDef.DataType)
		}
	}

	if encrypt {
		db.Tables[name].Encrypt = true

		// sha256 hash the key
		hash := sha256.New()

		// Write data to the hash
		hash.Write(key)

		// Calculate the hash
		hashBytes := hash.Sum(nil)

		db.Tables[name].HashedKey = [32]byte(hashBytes)

		// The nonce is 12 bytes of the end of the hash
		db.Tables[name].Nonce = [12]byte{}
		db.Tables[name].Nonce = [12]byte(append(db.Tables[name].Nonce[:], hashBytes[len(hashBytes)-12:]...))
	}

	if compress {
		db.Tables[name].Compress = true
	}

	// Create sequence file
	seqFile, err := os.Create(fmt.Sprintf("%s%s%s%s", db.Tables[name].Directory, shared.GetOsPathSeparator(), name, DB_SCHEMA_TABLE_SEQ_FILE_EXTENSION))
	if err != nil {
		delete(db.Tables, name)
		os.RemoveAll(fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), name))
		return err
	}

	schemaFile, err := os.Create(fmt.Sprintf("%s%s%s%s", db.Tables[name].Directory, shared.GetOsPathSeparator(), name, DB_SCHEMA_TABLE_SCHEMA_FILE_EXTENSION))
	if err != nil {
		delete(db.Tables, name)
		os.RemoveAll(fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), name))
		return err
	}

	defer schemaFile.Close()

	// Encode schema to file
	enc := gob.NewEncoder(schemaFile)

	err = enc.Encode(tblSchema)
	if err != nil {
		delete(db.Tables, name)
		os.RemoveAll(fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), name))
		return err
	}

	// Create btree pager
	rowFile, err := btree.OpenPager(fmt.Sprintf("%s%s%s%s", db.Tables[name].Directory, shared.GetOsPathSeparator(), name, DB_SCHEMA_TABLE_DATA_FILE_EXTENSION), os.O_CREATE|os.O_RDWR, 0755)
	if err != nil {
		delete(db.Tables, name)
		os.RemoveAll(fmt.Sprintf("%s%s%s", db.Directory, shared.GetOsPathSeparator(), name))
		return err
	}

	db.Tables[name].Rows = rowFile

	db.Tables[name].SequenceFile = seqFile
	db.Tables[name].SeqLock = &sync.Mutex{}

	return nil
}

// GetTable gets a table by name
func (db *Database) GetTable(tableName string) *Table {
	return db.Tables[tableName]
}

// CreateIndex creates a new index on a table
func (tbl *Table) CreateIndex(name string, columns []string, unique bool) error {
	if len(name) > MAX_INDEX_NAME_SIZE {
		return fmt.Errorf("index name is too long, max length is %d", MAX_INDEX_NAME_SIZE)
	}

	// Check if index exists
	if _, ok := tbl.Indexes[name]; ok {
		return fmt.Errorf("index %s already exists", name)
	}

	bt, err := btree.Open(fmt.Sprintf("%s%s%s%s", tbl.Directory, shared.GetOsPathSeparator(), fmt.Sprintf("idx_%s", name), ".bt"), os.O_CREATE|os.O_RDWR, 0755, 6)
	if err != nil {
		return err
	}

	// Create index
	tbl.Indexes[name] = &Index{
		Name:    name,
		Columns: columns,
		Unique:  unique,
		btree:   bt,
		lock:    &sync.Mutex{},
	}

	// Create index file
	indexFile, err := os.Create(fmt.Sprintf("%s%s%s%s", tbl.Directory, shared.GetOsPathSeparator(), fmt.Sprintf("idx_%s", name), DB_SCHEMA_TABLE_INDEX_FILE_EXTENSION))
	if err != nil {
		return err
	}

	defer indexFile.Close()

	// Encode index to file
	enc := gob.NewEncoder(indexFile)

	err = enc.Encode(tbl.Indexes[name])
	if err != nil {
		return err
	}

	return nil

}

// DropIndex drops an index by name
func (tbl *Table) DropIndex(name string) error {
	// Check if index exists
	if _, ok := tbl.Indexes[name]; !ok {
		return fmt.Errorf("index %s does not exist", name)
	}

	// Drop index
	delete(tbl.Indexes, name)

	// Drop index file
	err := os.Remove(fmt.Sprintf("%s%s%s%s", tbl.Directory, shared.GetOsPathSeparator(), fmt.Sprintf("idx_%s", name), DB_SCHEMA_TABLE_INDEX_FILE_EXTENSION))
	if err != nil {
		return err
	}

	// Remove btree file
	err = os.Remove(fmt.Sprintf("%s%s%s%s", tbl.Directory, shared.GetOsPathSeparator(), fmt.Sprintf("idx_%s", name), ".bt"))
	if err != nil {
		return err
	}

	return nil
}

// GetDatabase gets a database by name
func (cat *Catalog) GetDatabase(name string) *Database {

	return cat.Databases[name]

	return nil
}

// GetIndex gets an index by name
func (tbl *Table) GetIndex(name string) *Index {
	return tbl.Indexes[name]
}

// Insert inserts a row into the table
func (tbl *Table) Insert(rows []map[string]interface{}, db *Database) ([]int64, []map[string]interface{}, error) {
	rowIds := make([]int64, 0)                        // inserted row ids
	insertedRows := make([]map[string]interface{}, 0) // inserted rows

	for _, row := range rows {
		// Insert row into table
		rowId, err := tbl.insert(row, db)
		if err != nil {
			return nil, nil, err
		}

		rowIds = append(rowIds, rowId)
		insertedRows = append(insertedRows, row)

	}

	return rowIds, insertedRows, nil
}

// insert inserts a row into the table
func (tbl *Table) insert(row map[string]interface{}, db *Database) (int64, error) {
	// Check row against schema
	for colName, colDef := range tbl.TableSchema.ColumnDefinitions {

		if colDef.NotNull && !colDef.Sequence {
			if _, ok := row[colName]; !ok {
				return -1, fmt.Errorf("column %s cannot be null", colName)
			}
		}

		// Check if row column exists if not set to NULL
		if _, ok := row[colName]; !ok {
			row[colName] = nil
		}

		switch strings.ToUpper(colDef.DataType) {
		case "TEXT":
			if _, ok := row[colName].(string); !ok {
				return -1, fmt.Errorf("column %s is not a string", colName)
			}

		case "BOOL", "BOOLEAN":
			if _, ok := row[colName].(bool); !ok {
				return -1, fmt.Errorf("column %s is not a boolean", colName)
			}
		case "BLOB":
			if _, ok := row[colName].(string); !ok {
				return -1, fmt.Errorf("column %s is not a string", colName)
			}

			var err error

			// Decode hex (0x0102030405060708090A0B0C0D0E0F10)
			row[colName], err = hex.DecodeString(row[colName].(string))
			if err != nil {
				return -1, fmt.Errorf("column %s is not a valid binary", colName)
			}
		case "BINARY":
			if _, ok := row[colName].(string); !ok {
				return -1, fmt.Errorf("column %s is not a string", colName)
			}

			// Check length
			if len(row[colName].(string)) > colDef.Length {
				return -1, fmt.Errorf("column %s is too long", colName)
			}

			var err error

			// Decode hex (0x0102030405060708090A0B0C0D0E0F10)
			row[colName], err = hex.DecodeString(row[colName].(string))
			if err != nil {
				return -1, fmt.Errorf("column %s is not a valid binary", colName)
			}

		case "UUID":
			if colDef.NotNull {
				return -1, fmt.Errorf("column %s is not a string", colName)
			} else if colDef.Default != nil {
				if _, ok := colDef.Default.(*shared.GenUUID); ok {
					row[colName] = uuid.New().String()
				} else {
					continue
				}
			}

			// Check if valid UUID
			_, err := uuid.Parse(row[colName].(string))
			if err != nil {
				return -1, errors.New(fmt.Sprintf("'%s' is not a valid UUID\n", row[colName].(string)))
			}
		case "DATETIME", "TIMESTAMP":
			if _, ok := row[colName].(string); !ok {
				if colDef.NotNull {
					return -1, fmt.Errorf("column %s is not a string", colName)
				} else if colDef.Default != nil {
					if _, ok := colDef.Default.(*shared.SysDate); ok {
						row[colName] = time.Now()
					} else if _, ok := colDef.Default.(*shared.SysTime); ok {
						row[colName] = time.Now()
					} else if _, ok := colDef.Default.(*shared.SysTimestamp); ok {
						row[colName] = time.Now()
					}

					continue
				} else {
					continue
				}
			}

			// Check date format
			// Should be in the format YYYY-MM-DD HH:MM:SS

			// convert 2024-09-14 153201 to 2024-09-14 15:32:01
			row[colName] = strings.TrimSuffix(strings.TrimPrefix(row[colName].(string), "'"), "'")

			original := row[colName].(string)

			// Split the date and time parts
			datePart := original[:10]
			timePart := original[11:]

			// Extract hours, minutes, and seconds
			hours := timePart[:2]
			minutes := timePart[2:4]
			seconds := timePart[4:]

			// Format the new datetime string
			row[colName] = fmt.Sprintf("%s %s:%s:%s", datePart, hours, minutes, seconds)

			if !shared.IsValidDateTimeFormat(row[colName].(string)) {
				return -1, fmt.Errorf("column %s is not a valid datetime", colName)
			}

			// convert to time.Time
			t, err := shared.StringToGOTime(row[colName].(string))
			if err != nil {
				return -1, fmt.Errorf("column %s is not a valid datetime", colName)
			}

			row[colName] = t

		case "DATE":
			if _, ok := row[colName].(string); !ok {
				if colDef.NotNull {
					return -1, fmt.Errorf("column %s is not a string", colName)
				} else {
					continue
				}
			}

			// Check date format
			// Should be in the format YYYY-MM-DD
			if !shared.IsValidDateFormat(strings.TrimSuffix(strings.TrimPrefix(row[colName].(string), "'"), "'")) {
				return -1, fmt.Errorf("column %s is not a valid date", colName)
			}

			// convert to time.Time
			t, err := shared.StringToGOTime(strings.TrimSuffix(strings.TrimPrefix(row[colName].(string), "'"), "'"))
			if err != nil {
				return -1, fmt.Errorf("column %s is not a valid date", colName)
			}

			row[colName] = t

		case "TIME":
			if _, ok := row[colName].(string); !ok {
				if colDef.NotNull {
					return -1, fmt.Errorf("column %s is not a string", colName)
				} else {
					continue
				}
			}

			// Check date format
			// Should be in the format HH:MM:SS

			if !shared.IsValidTimeFormat(row[colName].(string)) {
				return -1, fmt.Errorf("column %s is not a valid time", colName)
			}

			// convert to time.Time
			t, err := shared.StringToGOTime(row[colName].(string))
			if err != nil {
				return -1, fmt.Errorf("column %s is not a valid date", colName)
			}

			row[colName] = t

		case "CHARACTER", "CHAR":
			if _, ok := row[colName].(string); !ok {

				// if column can be null, check if it is null
				if colDef.NotNull {
					if row[colName] != nil {
						return -1, fmt.Errorf("column %s is not a string", colName)
					}
				} else {
					continue
				}

			} else {
				// Check length
				if len(strings.TrimSuffix(strings.TrimPrefix(row[colName].(string), "'"), "'")) > colDef.Length {
					return -1, fmt.Errorf("column %s is too long", colName)
				}
			}

		case "NUMERIC", "DECIMAL", "DEC", "FLOAT", "DOUBLE", "REAL":
			if _, ok := row[colName].(float64); !ok {

				if colDef.NotNull {
					if row[colName] != nil {
						return -1, fmt.Errorf("column %s is not a floating point number", colName)
					}
				} else {
					continue
				}
			}

			str := fmt.Sprintf("%.14g", row[colName].(float64))

			// Split the string on the decimal point
			parts := strings.Split(str, ".")

			if len(parts) > 1 {

				// The scale is the number of digits after the decimal point
				scale := len(parts[1])

				// The precision is the total number of digits
				precision := len(parts[0]) + len(parts[1])

				if colDef.Scale > 0 {
					// Check scale

					if scale > colDef.Scale {
						return -1, fmt.Errorf("column %s has too many digits after the decimal point", colName)
					}

				}

				if colDef.Precision > 0 {
					// Check precision
					if precision > colDef.Precision {
						return -1, fmt.Errorf("column %s is too large", colName)
					}
				}
			}

		case "INT", "INTEGER", "SMALLINT":
			// Check for sequence
			if colDef.Sequence {
				// Check if sequence column is unique
				idx := tbl.CheckIndexedColumn(colName, true)
				if idx == nil {
					return -1, fmt.Errorf("sequence column %s must be unique", colName)
				}

				// Increment sequence
				seq, err := tbl.IncrementSequence()
				if err != nil {
					return -1, err
				}

				row[colName] = seq
			}

			if _, ok := row[colName].(int); !ok {
				if _, ok := row[colName].(uint64); !ok {
					return -1, fmt.Errorf("column %s is not an int", colName)
				} else {
					row[colName] = int(row[colName].(uint64))
				}

			}

			// Check if value fits in either INT/INTEGER, SMALLINT

			// Check if value fits in INT/INTEGER
			if strings.ToUpper(colDef.DataType) == "INT" || strings.ToUpper(colDef.DataType) == "INTEGER" {
				if row[colName].(int) > 2147483647 {
					return -1, fmt.Errorf("column %s is too large for INT/INTEGER", colName)
				}
			}

			// Check if value fits in SMALLINT
			if strings.ToUpper(colDef.DataType) == "SMALLINT" {
				if row[colName].(int) > 32767 {
					return -1, fmt.Errorf("column %s is too large for SMALLINT", colName)
				}
			}
		default:
			return -1, fmt.Errorf("invalid data type %s", colDef.DataType)
		}

		if colDef.Unique {
			// Check if unique key exists
			if !colDef.Sequence {
				if _, ok := row[colName]; !ok {
					return -1, fmt.Errorf("column %s cannot be null", colName)
				}
			}

			idx := tbl.CheckIndexedColumn(colName, true)
			if idx == nil {
				return -1, fmt.Errorf("problem getting unique rows for column %s", colName)
			}

			// Check if unique key exists
			key, err := idx.btree.Get([]byte(fmt.Sprintf("%v", row[colName])))
			if err != nil {
				return -1, fmt.Errorf("problem getting unique rows for column %s", colName)
			}

			if key != nil {

				for _, rowId := range key.V {
					// We store a []byte(rowId) in the btree
					// We need to convert it to an int64

					// Convert []byte to int64
					id, err := strconv.ParseInt(string(rowId), 10, 64)
					if err != nil {
						return -1, errors.New("problem getting unique rows")
					}

					// Get row from table
					r, err := tbl.Rows.GetPage(id)
					if err != nil {
						return -1, errors.New("problem getting unique rows")
					}

					// Decode row
					decoded, err := decodeRow(r)
					if err != nil {
						return -1, errors.New("problem getting unique rows")
					}

					// Check if row exists
					if decoded[colName] == row[colName] {
						return -1, fmt.Errorf("row with %s %v already exists", colName, row[colName])
					}

				}
			}

		}

		if colDef.References != nil {
			// Check if foreign key exists
			if _, ok := row[colName]; !ok {
				return -1, fmt.Errorf("column %s cannot be null", colName)
			}

			// Get referenced table
			refTbl := db.GetTable(colDef.References.TableName)
			if refTbl == nil {
				return -1, fmt.Errorf("foreign key constraint violation on column %s", colName)
			}

			// Check if foreign key exists
			idx := refTbl.CheckIndexedColumn(colName, true)
			if idx == nil {
				return -1, fmt.Errorf("foreign key constraint violation on column %s", colName)
			}

			if idx == nil {
				return -1, fmt.Errorf("foreign key constraint violation on column %s", colName)

			}

		}

	}

	// Write row to table
	rowId, err := tbl.writeRow(row)
	if err != nil {
		return -1, err
	}

	// Insert row into indexes
	for col, val := range row {
		for _, idx := range tbl.Indexes {
			if slices.Contains(idx.Columns, col) {

				// Check for compression
				if tbl.Compress {
					val, err = Compress([]byte(fmt.Sprintf("%v", val)))
					if err != nil {
						return -1, err
					}
				}

				if tbl.Encrypt {
					val, err = Encrypt(tbl.HashedKey, tbl.Nonce, val.([]byte))
					if err != nil {
						return -1, err
					}
				}

				err := idx.btree.Put([]byte(fmt.Sprintf("%v", val)), []byte(fmt.Sprintf("%d", rowId)))
				if err != nil {
					return -1, err
				}
			}
		}
	}

	return rowId, nil
}

// GetBtree gets the btree for an index
func (idx *Index) GetBtree() *btree.BTree {
	return idx.btree
}

// writeRow writes a row to the table
func (tbl *Table) writeRow(row map[string]interface{}) (int64, error) {
	// Write row to table

	// encode row to bytes
	encoded, err := EncodeRow(row)
	if err != nil {
		return -1, err
	}

	// check if table has compression set
	if tbl.Compress {
		// compress row
		encoded, err = Compress(encoded)
		if err != nil {
			return -1, err
		}

	}

	// Check if table has encryption set
	if tbl.Encrypt {
		// encrypt row
		encoded, err = Encrypt(tbl.HashedKey, tbl.Nonce, encoded)
		if err != nil {
			return -1, err
		}
	}

	rowId, err := tbl.Rows.Write(encoded)
	if err != nil {
		return -1, err
	}

	return rowId, nil
}

// EncodeRow encodes a row to a byte slice
func EncodeRow(n map[string]interface{}) ([]byte, error) {
	// use gob
	buff := new(bytes.Buffer)

	enc := gob.NewEncoder(buff)
	err := enc.Encode(n)

	if err != nil {
		return nil, err

	}

	return buff.Bytes(), nil
}

// decodeRow decodes a row from a byte slice
func decodeRow(b []byte) (map[string]interface{}, error) {
	var decoded map[string]interface{}

	err := gob.NewDecoder(bytes.NewReader(b)).Decode(&decoded)
	if err != nil {
		return nil, err
	}

	return decoded, nil
}

// IncrementSequence increments the sequence for the table
func (tbl *Table) IncrementSequence() (int, error) {
	tbl.SeqLock.Lock()
	defer tbl.SeqLock.Unlock()
	d, err := os.ReadFile(tbl.SequenceFile.Name())

	if string(d) == "" {
		tbl.SequenceFile.Write([]byte("1"))
		return 1, nil
	}

	i, err := strconv.Atoi(string(d))
	if err != nil {
		return 0, err
	}

	j := i + 1
	tbl.SequenceFile.Truncate(0)
	tbl.SequenceFile.Seek(0, os.SEEK_SET)
	tbl.SequenceFile.Write([]byte(fmt.Sprintf("%d", j)))

	return j, nil

	return 0, nil
}

// Iterator is an iterator for rows in a table
type Iterator struct {
	table *Table
	row   int64
}

// GetTable gets the table for the iterator
func (ri *Iterator) GetTable() *Table {
	return ri.table
}

// GetRow gets a row by id
func (tbl *Table) GetRow(rowId int64) (map[string]interface{}, error) {
	// Read row from table
	row, err := tbl.Rows.GetPage(rowId)
	if err != nil {
		return nil, err
	}

	// check for encryption
	if tbl.Encrypt {
		row, err = Decrypt(tbl.HashedKey, tbl.Nonce, row)
		if err != nil {
			return nil, err
		}
	}

	if tbl.Compress {
		row, err = Decompress(row)
		if err != nil {
			return nil, err
		}
	}

	// decode row
	decoded, err := decodeRow(row)
	if err != nil {
		return nil, err
	}

	return decoded, nil
}

// NewIterator returns a new row iterator
func (tbl *Table) NewIterator() *Iterator {
	return &Iterator{
		table: tbl,
		row:   0,
	}
}

// Current returns the current row id
func (ri *Iterator) Current() int64 {
	return ri.row
}

// Next returns the next row in the table
func (ri *Iterator) Next() (map[string]interface{}, error) {
	for {
		if slices.Contains(ri.table.Rows.GetDeletedPages(), ri.row) {
			ri.row++
			continue

		} else {
			break
		}

	}

	// Read row from table
	row, err := ri.table.Rows.GetPage(ri.row)
	if err != nil {
		return nil, err
	}

	// decode row
	decoded, err := decodeRow(row)
	if err != nil {
		ri.row++
		// When decoding next a row can be an overflow or deleted that is why we skip it
		return nil, nil
	}

	ri.row++

	return decoded, nil
}

// Valid returns true if the iterator is valid
func (ri *Iterator) Valid() bool {
	return ri.row < ri.table.Rows.Count()

}

// IOCount returns the amount of IO operations
func (tbl *Table) IOCount() int64 {
	return tbl.Rows.Count() // This is not correct amount of rows as each page can be an overflow or deleted, this is just amount trips to disk
}

// CheckIndexedColumn checks if a column is indexed, if so return index
// If unique is true, check if the index is unique
func (tbl *Table) CheckIndexedColumn(column string, unique bool) *Index {
	for _, idx := range tbl.Indexes {
		if slices.Contains(idx.Columns, column) {

			if idx.Unique == unique {
				return idx
			}
		}
	}

	return nil
}

// GetUniqueIndex gets the first unique index for a table
func (tbl *Table) GetUniqueIndex() *Index {
	for _, idx := range tbl.Indexes {
		if idx.Unique {
			return idx
		}
	}

	return nil

}

// DeleteRow deletes a row from the table
func (tbl *Table) DeleteRow(rowId int64) error {
	// Read row from table
	row, err := tbl.Rows.GetPage(rowId)
	if err != nil {
		return err
	}

	// decode row
	decoded, err := decodeRow(row)
	if err != nil {
		return err
	}

	// Delete row from indexes
	for col, val := range decoded {
		for _, idx := range tbl.Indexes {
			if slices.Contains(idx.Columns, col) {
				// Remove from index
				err := idx.btree.Remove([]byte(fmt.Sprintf("%v", val)), []byte(fmt.Sprintf("%d", rowId)))
				if err != nil {
					return err
				}
			}
		}
	}

	// Delete row from table
	err = tbl.Rows.DeletePage(rowId)
	if err != nil {
		return err
	}

	return nil
}

// SetClause Set for update
type SetClause struct {
	ColumnName string
	Value      interface{}
}

// CopyRow copies a row
func CopyRow(row *map[string]interface{}) map[string]interface{} {
	newRow := make(map[string]interface{})
	for k, v := range *row {
		newRow[k] = v
	}
	return newRow
}

// UpdateRow updates a row in the table
func (tbl *Table) UpdateRow(rowId int64, row map[string]interface{}, sets []*SetClause) error {

	var prevRow map[string]interface{}

	for _, set := range sets {

		if _, ok := row[set.ColumnName]; !ok {
			return fmt.Errorf("column %s does not exist", set.ColumnName)
		}

		prevRow = CopyRow(&row)
		row[set.ColumnName] = set.Value

		// Check row against schema
		for colName, colDef := range tbl.TableSchema.ColumnDefinitions {
			if colName == set.ColumnName {
				switch strings.ToUpper(colDef.DataType) {
				case "CHARACTER", "CHAR":
					if _, ok := row[colName].(string); !ok {
						if !colDef.NotNull {
							if row[colName] != nil {
								return fmt.Errorf("column %s is not a string", colName)
							}
						}
					} else {
						// Check length
						if len(row[colName].(string)) > colDef.Length {
							return fmt.Errorf("column %s is too long", colName)
						}
					}

				case "NUMERIC", "DECIMAL", "DEC", "FLOAT", "DOUBLE", "REAL":
					if _, ok := row[colName].(float64); !ok {
						return fmt.Errorf("column %s is not a float64", colName)
					}

					str := fmt.Sprintf("%.14g", row[colName].(float64))

					// Split the string on the decimal point
					parts := strings.Split(str, ".")

					if len(parts) > 1 {

						// The scale is the number of digits after the decimal point
						scale := len(parts[1])

						// The precision is the total number of digits
						precision := len(parts[0]) + len(parts[1])

						if colDef.Scale > 0 {
							// Check scale

							if scale > colDef.Scale {
								return fmt.Errorf("column %s has too many digits after the decimal point", colName)
							}

						}

						if colDef.Precision > 0 {
							// Check precision
							if precision > colDef.Precision {
								return fmt.Errorf("column %s is too large", colName)
							}
						}
					}

				case "INT", "INTEGER", "SMALLINT":

					if _, ok := row[colName].(int); !ok {
						if _, ok := row[colName].(uint64); !ok {
							return fmt.Errorf("column %s is not an int", colName)
						} else {
							row[colName] = int(row[colName].(uint64))
						}
					}

					// Check if value fits in INT/INTEGER
					if strings.ToUpper(colDef.DataType) == "INT" || strings.ToUpper(colDef.DataType) == "INTEGER" {
						if row[colName].(int) > 2147483647 {
							return fmt.Errorf("column %s is too large for INT/INTEGER", colName)
						}
					}

					// Check if value fits in SMALLINT
					if strings.ToUpper(colDef.DataType) == "SMALLINT" {
						if row[colName].(int) > 32767 {
							return fmt.Errorf("column %s is too large for SMALLINT", colName)
						}
					}

				}

			}
		}

	}

	// Encode row
	encoded, err := EncodeRow(row)
	if err != nil {
		return err
	}

	err = tbl.Rows.WriteTo(rowId, encoded)
	if err != nil {
		return err
	}

	for _, set := range sets {
		for colName, _ := range tbl.TableSchema.ColumnDefinitions {
			if colName == set.ColumnName {
				for _, idx := range tbl.Indexes {
					if slices.Contains(idx.Columns, colName) {
						// Remove old value from index
						err := idx.btree.Remove([]byte(fmt.Sprintf("%v", prevRow[colName])), []byte(fmt.Sprintf("%d", rowId)))
						if err != nil {
							return err
						}

						// Insert into index
						err = idx.btree.Put([]byte(fmt.Sprintf("%v", row[colName])), []byte(fmt.Sprintf("%d", rowId)))
						if err != nil {
							return err
						}
					}
				}
			}
		}
	}

	return nil

}

// RevokePrivilegeFromUser revokes a privilege from a user
func (cat *Catalog) RevokePrivilegeFromUser(username string, priv *Privilege) error {
	// Lock users map
	cat.UsersLock.Lock()
	defer cat.UsersLock.Unlock()

	// Check if user exists
	if _, ok := cat.Users[username]; !ok {
		return fmt.Errorf("user %s does not exist", username)
	}

	// Check if privilege exists
	for _, p := range cat.Users[username].Privileges {
		if p.DatabaseName == priv.DatabaseName && p.TableName == priv.TableName {

			// Revoke privilege
			for i, l := range cat.Users[username].Privileges {
				if l.DatabaseName == l.DatabaseName && l.TableName == l.TableName {

					if len(l.PrivilegeActions) == len(priv.PrivilegeActions) {

						cat.Users[username].Privileges = append(cat.Users[username].Privileges[:i], cat.Users[username].Privileges[i+1:]...)
					} else {
						for _, a := range priv.PrivilegeActions {
							for j, b := range l.PrivilegeActions {
								if a == b {
									// only remove the privilege action
									cat.Users[username].Privileges[i].PrivilegeActions = append(l.PrivilegeActions[:j], l.PrivilegeActions[j+1:]...)
								}
							}
						}
					}
					break
				}
			}

			err := cat.EncodeUsersToFile()

			if err != nil {
				return err
			}

			return nil

		}

	}

	return fmt.Errorf("privilege does not exist for user %s", username)
}

// GrantPrivilegeToUser grants a privilege to a user
func (cat *Catalog) GrantPrivilegeToUser(username string, priv *Privilege) error {
	// Lock users map
	cat.UsersLock.Lock()
	defer cat.UsersLock.Unlock()

	// Check if user exists
	if _, ok := cat.Users[username]; !ok {
		return fmt.Errorf("user %s does not exist", username)
	}

	// Check if privilege exists
	for _, p := range cat.Users[username].Privileges {
		if p.DatabaseName == priv.DatabaseName && p.TableName == priv.TableName {
			return fmt.Errorf(fmt.Sprintf("privilege already exists for user %s", username))
		}
	}

	cat.Users[username].Privileges = append(cat.Users[username].Privileges, priv)

	err := cat.EncodeUsersToFile()

	if err != nil {
		return err
	}

	return nil

}

// DropUser removes a user
func (cat *Catalog) DropUser(username string) error {
	// Lock users map
	cat.UsersLock.Lock()
	defer cat.UsersLock.Unlock()

	// Check if user exists
	if _, ok := cat.Users[username]; !ok {
		return fmt.Errorf("user %s does not exist", username)
	}

	// Drop user
	delete(cat.Users, username)

	err := cat.EncodeUsersToFile()
	if err != nil {
		return err
	}

	return nil

}

// CreateNewUser creates a new user
func (cat *Catalog) CreateNewUser(username, password string) error {
	// Lock users map
	cat.UsersLock.Lock()
	defer cat.UsersLock.Unlock()

	// Check if user exists
	if _, ok := cat.Users[username]; ok {
		return fmt.Errorf("user %s already exists", username)
	}

	// bcrypt password
	hashedPassword, err := shared.HashPassword(password)
	if err != nil {
		return err
	}

	// Create user
	cat.Users[username] = &User{
		Username: username,
		Password: hashedPassword,
	}

	err = cat.EncodeUsersToFile()
	if err != nil {
		return err
	}

	return nil

}

// EncodeUsersToFile encodes users to file
func (cat *Catalog) EncodeUsersToFile() error {
	// Lock users file
	cat.UsersFileLock.Lock()
	defer cat.UsersFileLock.Unlock()

	// seek to beginning of file
	if _, err := cat.UsersFile.Seek(0, 0); err != nil {
		return err
	}

	// Encode users to file
	enc := gob.NewEncoder(cat.UsersFile)

	err := enc.Encode(cat.Users)
	if err != nil {
		return err
	}

	return nil
}

// ReadUsersFromFile reads users from file
func (cat *Catalog) ReadUsersFromFile() error {

	if _, err := cat.UsersFile.Seek(0, 0); err != nil {
		return err
	}

	// Check size
	fi, err := cat.UsersFile.Stat()
	if err != nil {
		return err
	}

	if fi.Size() == 0 {

		return errors.New("users file is empty")
	}

	// Lock users file
	cat.UsersFileLock.Lock()
	defer cat.UsersFileLock.Unlock()

	// Read users from file
	dec := gob.NewDecoder(cat.UsersFile)

	err = dec.Decode(&cat.Users)
	if err != nil {
		return err
	}

	return nil
}

// GetUser gets a user by username
func (cat *Catalog) GetUser(username string) *User {
	cat.UsersLock.Lock()
	defer cat.UsersLock.Unlock()
	return cat.Users[username]
}

// AuthenticateUser authenticates a user
func (cat *Catalog) AuthenticateUser(username, password string) (*User, error) {
	cat.UsersLock.Lock()
	defer cat.UsersLock.Unlock()

	// Check if user exists
	if _, ok := cat.Users[username]; !ok {
		return nil, fmt.Errorf("user %s does not exist", username)
	}

	// Check password
	ok := shared.ComparePasswords(cat.Users[username].Password, password)
	if !ok {
		return nil, errors.New("authentication failed")
	}

	return cat.Users[username], nil
}

// HasPrivilege checks if a user has a privilege
func (u *User) HasPrivilege(db, tbl string, actions []shared.PrivilegeAction) bool {

	var has []bool // Slice of booleans determining if user has privileges

	for _, p := range u.Privileges {
		for _, a := range actions {
			if p.DatabaseName == db && p.TableName == tbl { // if the requested database and table match the privilege
				for _, pa := range p.PrivilegeActions {
					if pa == a { // if the privilege action matches the requested action
						has = append(has, true)
					} else if pa == shared.PRIV_ALL { // if the privilege action is all
						has = append(has, true)
					}
				}
			} else if p.DatabaseName == "*" && p.TableName == "*" { // all databases and tables
				for _, pa := range p.PrivilegeActions {
					if pa == a {
						has = append(has, true)
					} else if pa == shared.PRIV_ALL {
						has = append(has, true)
					}
				}
			} else if p.DatabaseName == db && p.TableName == "*" { // all tables in a database
				for _, pa := range p.PrivilegeActions {
					if pa == a {
						has = append(has, true)
					} else if pa == shared.PRIV_ALL {
						has = append(has, true)
					}
				}
			} else if p.DatabaseName == "*" && p.TableName == tbl { // all databases and a table
				for _, pa := range p.PrivilegeActions {
					if pa == a {
						has = append(has, true)
					} else if pa == shared.PRIV_ALL {
						has = append(has, true)
					}
				}
			}
		}
	}

	if len(has) == len(actions) {
		return true
	}

	return false
}

// GetUsers gets all users
func (cat *Catalog) GetUsers() []string {
	cat.UsersLock.Lock()
	defer cat.UsersLock.Unlock()

	var users []string
	for k := range cat.Users {
		users = append(users, k)
	}

	slices.Sort(users)

	return users
}

// GetTables gets all tables in a database
func (db *Database) GetTables() []string {

	db.TablesLock.Lock()
	defer db.TablesLock.Unlock()

	var tables []string
	for k := range db.Tables {
		tables = append(tables, k)
	}

	return tables
}

// GetDatabases gets all databases
func (cat *Catalog) GetDatabases() []string {
	cat.DatabasesLock.Lock()
	defer cat.DatabasesLock.Unlock()

	var dbs []string
	for k := range cat.Databases {
		dbs = append(dbs, k)
	}

	slices.Sort(dbs)

	return dbs
}

// AlterUserUsername alters a user's username
func (cat *Catalog) AlterUserUsername(oldUsername, newUsername string) error {
	// Lock users map
	cat.UsersLock.Lock()
	defer cat.UsersLock.Unlock()

	// Check if user exists
	if _, ok := cat.Users[oldUsername]; !ok {
		return fmt.Errorf("user %s does not exist", oldUsername)
	}

	// Check if new username already exists
	if _, ok := cat.Users[newUsername]; ok {
		return fmt.Errorf("user %s already exists", newUsername)
	}

	// Create user
	cat.Users[newUsername] = cat.Users[oldUsername]
	cat.Users[newUsername].Username = newUsername

	// Drop old user
	delete(cat.Users, oldUsername)

	err := cat.EncodeUsersToFile()
	if err != nil {
		return err
	}

	return nil

}

// AlterUserPassword alters a user's password
func (cat *Catalog) AlterUserPassword(username, password string) error {
	// Lock users map
	cat.UsersLock.Lock()
	defer cat.UsersLock.Unlock()

	// Check if user exists
	if _, ok := cat.Users[username]; !ok {
		return fmt.Errorf("user %s does not exist", username)
	}

	// bcrypt password
	hashedPassword, err := shared.HashPassword(password)
	if err != nil {
		return err
	}

	// Create user
	cat.Users[username].Password = hashedPassword

	err = cat.EncodeUsersToFile()
	if err != nil {
		return err
	}

	return nil
}

// GetPrivileges gets a user's privileges
func (u *User) GetPrivileges() []string {
	var formattedStr []string

	for _, priv := range u.Privileges {
		var privActionsStr []string

		for _, action := range priv.PrivilegeActions {
			privActionsStr = append(privActionsStr, action.String())
		}

		formattedStr = append(formattedStr, fmt.Sprintf("%s.%s: %s", priv.DatabaseName, priv.TableName, strings.Join(privActionsStr, ", ")))

	}

	return formattedStr
}

// AddProcedure adds a procedure to the database
func (db *Database) AddProcedure(proc *Procedure) error {
	db.ProceduresFileLock.Lock()
	defer db.ProceduresFileLock.Unlock()

	if _, ok := db.Procedures[proc.Name]; ok {
		return fmt.Errorf("procedure %s already exists", proc.Name)
	}

	db.Procedures[proc.Name] = proc

	// Encode procedures to file
	err := db.EncodeProceduresToFile()
	if err != nil {
		return err

	}

	return nil
}

// DropProcedure drops a procedure from the database
func (db *Database) DropProcedure(procName string) error {
	db.ProceduresFileLock.Lock()
	defer db.ProceduresFileLock.Unlock()

	if _, ok := db.Procedures[procName]; !ok {
		return fmt.Errorf("procedure %s does not exist", procName)
	}

	delete(db.Procedures, procName)

	// Encode procedures to file
	err := db.EncodeProceduresToFile()
	if err != nil {
		return err

	}

	return nil
}

// GetProcedures gets all procedures in a database
func (db *Database) GetProcedures() []string {
	db.ProceduresFileLock.Lock()
	defer db.ProceduresFileLock.Unlock() // Lock procedures file which also locks procedures map

	var procs []string
	for k := range db.Procedures {
		procs = append(procs, k)
	}

	slices.Sort(procs)

	return procs
}

// GetProcedure gets a procedure by name
func (db *Database) GetProcedure(procName string) (*Procedure, error) {
	db.ProceduresFileLock.Lock()
	defer db.ProceduresFileLock.Unlock()

	if _, ok := db.Procedures[procName]; !ok {
		return nil, fmt.Errorf("procedure %s does not exist", procName)
	}

	return db.Procedures[procName], nil
}

// EncodeProceduresToFile encodes procedures to file
func (db *Database) EncodeProceduresToFile() error {

	// seek to beginning of file
	if _, err := db.ProceduresFile.Seek(0, 0); err != nil {
		return err
	}

	// Encode procedures to file
	enc := gob.NewEncoder(db.ProceduresFile)

	err := enc.Encode(db.Procedures)
	if err != nil {
		return err
	}

	return nil
}

// Compress compresses a row with ZSTD
func Compress(row []byte) ([]byte, error) {
	return zstd.Compress(nil, row)

}

// Decompress decompresses a row with ZSTD
func Decompress(row []byte) ([]byte, error) {
	return zstd.Decompress(nil, row)
}

// Encrypt encrypts a row with ChaCha20
func Encrypt(key [32]byte, nonce [12]byte, row []byte) ([]byte, error) {
	var ciphertext = make([]byte, len(row))

	// Create a new ChaCha20 cipher
	c, err := chacha20.NewUnauthenticatedCipher(key[:], nonce[:])
	if err != nil {
		return nil, err
	}

	// Encrypt the plaintext
	c.XORKeyStream(ciphertext, row)
	return ciphertext, nil
}

// Decrypt decrypts ciphertext using ChaCha20
func Decrypt(key [32]byte, nonce [12]byte, cipherRow []byte) ([]byte, error) {
	var plaintext = make([]byte, len(cipherRow))

	// Create a new ChaCha20 cipher
	c, err := chacha20.NewUnauthenticatedCipher(key[:], nonce[:])
	if err != nil {
		return nil, err
	}

	// Decrypt the ciphertext
	c.XORKeyStream(plaintext, cipherRow)
	return plaintext, nil
}

// Alter alters a table, specifically a column
func (tbl *Table) Alter(columnName string, columnDef *ColumnDefinition) error {
	if columnDef == nil {
		// Drop column
		var existingIndexValues *Index

		// Find indexes that are using that column
		for _, idx := range tbl.Indexes {
			// if the index is just using that column drop the index
			// if the index is using multiple columns, remove the column from the index

			if slices.Contains(idx.Columns, columnName) {
				if len(idx.Columns) == 1 {
					// Drop index
					err := tbl.DropIndex(idx.Name)
					if err != nil {
						return err
					}
				} else {
					// Remove column from index
					for i, col := range idx.Columns {
						if col == columnName {
							idx.Columns = append(idx.Columns[:i], idx.Columns[i+1:]...)

							if !idx.Unique {
								existingIndexValues = idx
							}
						}

					}

				}
			}
		}

		// Drop column from schema
		delete(tbl.TableSchema.ColumnDefinitions, columnName)

		// iterate over all rows and remove the column
		ri := tbl.NewIterator()

		for ri.Valid() {
			row, err := ri.Next()
			if err != nil {
				continue
			}

			if _, ok := row[columnName]; ok {
				if existingIndexValues != nil {
					// remove from indexes
					existingIndexValues.btree.Remove([]byte(fmt.Sprintf("%v", row[columnName])), []byte(fmt.Sprintf("%d", ri.Current())))
				}
			}

			// Remove column from row
			delete(row, columnName)

			encoded, err := EncodeRow(row)
			if err != nil {
				continue
			}

			if tbl.Compress {
				encoded, err = Compress(encoded)
				if err != nil {
					continue
				}
			}

			if tbl.Encrypt {
				encoded, err = Encrypt(tbl.HashedKey, tbl.Nonce, encoded)
				if err != nil {
					continue
				}
			}

			// Write row back to table
			err = tbl.Rows.WriteTo(ri.Current(), encoded)
			if err != nil {
				return err
			}
		}
	} else {
		// check if column exists
		if _, ok := tbl.TableSchema.ColumnDefinitions[columnName]; !ok {
			// Column does not exist, add column

			if len(columnName) > MAX_COLUMN_NAME_SIZE {
				return fmt.Errorf("column name is too long, max length is %d", MAX_COLUMN_NAME_SIZE)
			}

			if !shared.IsValidDataType(columnDef.DataType) {
				return fmt.Errorf("invalid data type %s", columnDef.DataType)
			}

			if columnDef.Unique {
				err := tbl.CreateIndex(fmt.Sprintf("unique_%s", columnName), []string{columnName}, true)
				if err != nil {
					return err
				}
			}

			if columnDef.Sequence {
				// Check if sequence column exists
				var sequenceDefined bool
				for _, colDef := range tbl.TableSchema.ColumnDefinitions {
					if colDef.Sequence {
						sequenceDefined = true
					}
				}

				if sequenceDefined {
					return errors.New("sequence column already defined")
				}

				// Sequenced column must be unique and not null

				if !columnDef.Unique || !columnDef.NotNull {

					return fmt.Errorf("sequence column %s must be unique and not null", columnName)
				}

				// Datatype MUST be an integer
				if strings.ToUpper(columnDef.DataType) != "INT" && strings.ToUpper(columnDef.DataType) != "INTEGER" {
					return fmt.Errorf("sequence column %s must be an integer", columnName)
				}

				sequenceDefined = true
			}

			switch strings.ToUpper(columnDef.DataType) {
			case "CHARACTER", "CHAR":
				// A character datatype requires a length
				if columnDef.Length == 0 {
					return fmt.Errorf("column %s requires a length", columnName)
				}
			case "NUMERIC", "DECIMAL", "DEC", "FLOAT", "DOUBLE", "REAL":
				// A numeric datatype requires a precision and scale
				if columnDef.Precision == 0 {
					return fmt.Errorf("column %s requires a precision", columnName)
				}

				if columnDef.Scale == 0 {
					return fmt.Errorf("column %s requires a scale", columnName)
				}
			case "INT", "INTEGER", "SMALLINT":
			case "DATE", "TIME", "TIMESTAMP", "DATETIME":
			case "BINARY":
			case "UUID":
			case "BOOLEAN", "BOOL":
			case "TEXT":
			case "BLOB":

			default:
				return fmt.Errorf("invalid data type %s", columnDef.DataType)
			}

			// update schema
			tbl.TableSchema.ColumnDefinitions[columnName] = columnDef

			// write schema to file
			schemaFile, err := os.Create(fmt.Sprintf("%s%s%s%s", tbl.Directory, shared.GetOsPathSeparator(), tbl.Name, DB_SCHEMA_TABLE_SCHEMA_FILE_EXTENSION))
			if err != nil {
				return err
			}

			defer schemaFile.Close()

			// Encode schema to file
			enc := gob.NewEncoder(schemaFile)

			err = enc.Encode(tbl.TableSchema)
			if err != nil {
				return err
			}

			// iterate over all rows and add the column
			ri := tbl.NewIterator()

			for ri.Valid() {
				row, err := ri.Next()
				if err != nil {
					continue
				}

				if columnDef.NotNull {
					if _, ok := row[columnName]; !ok {
						return fmt.Errorf("column %s cannot be null", columnName)
					}
				}

				if columnDef.Unique {
					if _, ok := row[columnName]; !ok {
						return fmt.Errorf("column %s cannot be null", columnName)
					}

					idx := tbl.CheckIndexedColumn(columnName, true)
					if idx == nil {
						return fmt.Errorf("problem getting unique rows for column %s", columnName)
					}

					if tbl.Compress {
						row[columnName], err = Compress([]byte(fmt.Sprintf("%v", row[columnName])))
						if err != nil {
							return err
						}
					}

					if tbl.Encrypt {
						row[columnName], err = Encrypt(tbl.HashedKey, tbl.Nonce, row[columnName].([]byte))
						if err != nil {
							return err
						}
					}

					// Check if unique key exists
					key, err := idx.btree.Get([]byte(fmt.Sprintf("%v", row[columnName])))
					if err != nil {
						return fmt.Errorf("problem getting unique rows for column %s", columnName)
					}

					if key != nil {

						for _, rowId := range key.V {
							// We store a []byte(rowId) in the btree
							// We need to convert it to an int64

							// Convert []byte to int64
							id, err := strconv.ParseInt(string(rowId), 10, 64)
							if err != nil {
								return errors.New("problem getting unique rows")
							}

							// Get row from table
							r, err := tbl.Rows.GetPage(id)
							if err != nil {
								return errors.New("problem getting unique rows")
							}

							if tbl.Encrypt {
								r, err = Decrypt(tbl.HashedKey, tbl.Nonce, r)
								if err != nil {
									return errors.New("problem getting unique rows")
								}
							}

							if tbl.Compress {
								r, err = Decompress(r)
								if err != nil {
									return errors.New("problem getting unique rows")
								}
							}

							// Decode row
							decoded, err := decodeRow(r)
							if err != nil {
								return errors.New("problem getting unique rows")
							}

							// Check if row exists
							if decoded[columnName] == row[columnName] {
								return fmt.Errorf("row with %s %v already exists", columnName, row[columnName])
							}

						}
					}
				}

			}

		} else {
			return errors.New("you can only drop a column or add a new column")
		}
	}

	return nil

}
