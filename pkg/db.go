package pkg

import (
	"fmt"
	mysqlGO "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rancher/lasso/pkg/log"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"time"
)

type table_to_delete struct {
	ID   uint
	Name string `gorm:"size:255"`
}

type DatabaseManager struct {
	DB *gorm.DB
}

func NewDatabaseManager(dsn string) (*DatabaseManager, error) {
	var dialect gorm.Dialector
	switch {
	case dsn[:8] == "mysql://":
		dialect = mysql.Open(dsn[8:])
	case dsn[:11] == "postgres://":
		dialect = postgres.Open(dsn)
	case dsn[:8] == "sqlite://":
		dialect = sqlite.Open(dsn)
	default:
		return nil, fmt.Errorf("unsupported database dialect for dsn: %s", dsn)
	}

	db, err := gorm.Open(dialect, &gorm.Config{
		//Logger: logger.Default.LogMode(logger.Info),
	})
	if err != nil {
		return nil, err
	}

	err = db.AutoMigrate(&table_to_delete{})
	if err != nil {
		return nil, err
	}

	return &DatabaseManager{DB: db}, nil
}

func (dm *DatabaseManager) CreateDatabase(dbName string) error {
	// Replace all symbols in the database name
	result := dm.DB.Exec("CREATE DATABASE " + dbName)
	// Handle specific MySQL errors
	if result.Error != nil {
		if mysqlErr, ok := result.Error.(*mysqlGO.MySQLError); ok {
			switch mysqlErr.Number {
			case 1007: // MySQL code for duplicate entry
				log.Infof("Database %s already exists", dbName)
				return nil
			default:
				return result.Error
			}
		}
		if pqErr, ok := result.Error.(*pgconn.PgError); ok {
			switch pqErr.Code {
			case "42P04": // MySQL code for duplicate entry
				log.Infof("Database %s already exists", dbName)
				return nil
			default:
				return result.Error
			}
		}
		return result.Error
	}
	return result.Error
}

func (dm *DatabaseManager) AddDatabaseToRemove(dbName string) error {
	return dm.DB.Create(&table_to_delete{Name: dbName}).Error
}

func (dm *DatabaseManager) RemoveDatabases() error {
	var result []table_to_delete
	if err := dm.DB.Find(&result).Error; err != nil {
		return err
	}
	for _, database := range result {
		if database.Name != "" {
			resultD := dm.DB.Exec(fmt.Sprintf("DROP DATABASE %s", database.Name))
			if resultD.Error != nil {
				if mysqlErr, ok := resultD.Error.(*mysqlGO.MySQLError); ok {
					switch mysqlErr.Number {
					case 1008: // MySQL code for duplicate entry
						log.Infof("Database %s does not exist, skipping deletion", database.Name)
					default:
						return resultD.Error
					}
				}
				if pqErr, ok := resultD.Error.(*pgconn.PgError); ok {
					switch pqErr.Code {
					case "3D000": // MySQL code for duplicate entry
						log.Infof("Database %s does not exist, skipping deletion", database.Name)
					default:
						return resultD.Error
					}
				}
			}
			if err := dm.DB.Delete(&database).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func (dm *DatabaseManager) StartCronJob() {
	ticker := time.NewTicker(10 * time.Second)
	for {
		<-ticker.C
		if err := dm.RemoveDatabases(); err != nil {
			log.Errorf("Error removing databases: %v", err)
		}
	}
}
