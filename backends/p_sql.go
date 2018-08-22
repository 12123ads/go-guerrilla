package backends

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/flashmob/go-guerrilla/mail"

	"math/big"
	"net"
	"runtime/debug"

	"github.com/flashmob/go-guerrilla/response"
)

// ----------------------------------------------------------------------------------
// Processor Name: sql
// ----------------------------------------------------------------------------------
// Description   : Saves the e.Data (email data) and e.DeliveryHeader together in sql
//               : using the hash generated by the "hash" processor and stored in
//               : e.Hashes
// ----------------------------------------------------------------------------------
// Config Options: mail_table string - name of table for storing emails
//               : sql_driver string - database driver name, eg. mysql
//               : sql_dsn string - driver-specific data source name
//               : primary_mail_host string - primary host name
// --------------:-------------------------------------------------------------------
// Input         : e.Data
//               : e.DeliveryHeader generated by ParseHeader() processor
//               : e.MailFrom
//               : e.Subject - generated by by ParseHeader() processor
// ----------------------------------------------------------------------------------
// Output        : Sets e.QueuedId with the first item fromHashes[0]
// ----------------------------------------------------------------------------------
func init() {
	processors["sql"] = func() Decorator {
		return SQL()
	}
}

type SQLProcessorConfig struct {
	Table       string `json:"mail_table"`
	Driver      string `json:"sql_driver"`
	DSN         string `json:"sql_dsn"`
	PrimaryHost string `json:"primary_mail_host"`
}

type SQLProcessor struct {
	cache  stmtCache
	config *SQLProcessorConfig
}

func (s *SQLProcessor) connect() (*sql.DB, error) {
	var db *sql.DB
	var err error
	if db, err = sql.Open(s.config.Driver, s.config.DSN); err != nil {
		Log().Error("cannot open database: ", err)
		return nil, err
	}
	// do we have permission to access the table?
	_, err = db.Query("SELECT mail_id FROM " + s.config.Table + " LIMIT 1")
	if err != nil {
		return nil, err
	}
	return db, err
}

// prepares the sql query with the number of rows that can be batched with it
func (s *SQLProcessor) prepareInsertQuery(rows int, db *sql.DB) *sql.Stmt {
	if rows == 0 {
		panic("rows argument cannot be 0")
	}
	if s.cache[rows-1] != nil {
		return s.cache[rows-1]
	}
	sqlstr := "INSERT INTO " + s.config.Table + " "
	sqlstr += "(`date`, `to`, `from`, `subject`, `body`,  `mail`, `spam_score`, "
	sqlstr += "`hash`, `content_type`, `recipient`, `has_attach`, `ip_addr`, "
	sqlstr += "`return_path`, `is_tls`, `message_id`, `reply_to`, `sender`)"
	sqlstr += " VALUES "
	values := "(NOW(), ?, ?, ?, ? , ?, 0, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?)"
	// add more rows
	comma := ""
	for i := 0; i < rows; i++ {
		sqlstr += comma + values
		if comma == "" {
			comma = ","
		}
	}
	stmt, sqlErr := db.Prepare(sqlstr)
	if sqlErr != nil {
		Log().WithError(sqlErr).Panic("failed while db.Prepare(INSERT...)")
	}
	// cache it
	s.cache[rows-1] = stmt
	return stmt
}

func (s *SQLProcessor) doQuery(c int, db *sql.DB, insertStmt *sql.Stmt, vals *[]interface{}) (execErr error) {
	defer func() {
		if r := recover(); r != nil {
			Log().Error("Recovered form panic:", r, string(debug.Stack()))
			sum := 0
			for _, v := range *vals {
				if str, ok := v.(string); ok {
					sum = sum + len(str)
				}
			}
			Log().Errorf("panic while inserting query [%s] size:%d, err %v", r, sum, execErr)
			panic("query failed")
		}
	}()
	// prepare the query used to insert when rows reaches batchMax
	insertStmt = s.prepareInsertQuery(c, db)
	_, execErr = insertStmt.Exec(*vals...)
	if execErr != nil {
		Log().WithError(execErr).Error("There was a problem the insert")
	}
	return
}

// for storing ip addresses in the ip_addr column
func (s *SQLProcessor) ip2bint(ip string) *big.Int {
	bint := big.NewInt(0)
	addr := net.ParseIP(ip)
	if strings.Index(ip, "::") > 0 {
		bint.SetBytes(addr.To16())
	} else {
		bint.SetBytes(addr.To4())
	}
	return bint
}

func (s *SQLProcessor) fillAddressFromHeader(e *mail.Envelope, headerKey string) string {
	if v, ok := e.Header[headerKey]; ok {
		addr, err := mail.NewAddress(v[0])
		if err != nil {
			return ""
		}
		return addr.String()
	}
	return ""
}

func SQL() Decorator {
	var config *SQLProcessorConfig
	var vals []interface{}
	var db *sql.DB
	s := &SQLProcessor{}

	// open the database connection (it will also check if we can select the table)
	Svc.AddInitializer(InitializeWith(func(backendConfig BackendConfig) error {
		configType := BaseConfig(&SQLProcessorConfig{})
		bcfg, err := Svc.ExtractConfig(backendConfig, configType)
		if err != nil {
			return err
		}
		config = bcfg.(*SQLProcessorConfig)
		s.config = config
		db, err = s.connect()
		if err != nil {
			return err
		}
		return nil
	}))

	// shutdown will close the database connection
	Svc.AddShutdowner(ShutdownWith(func() error {
		if db != nil {
			return db.Close()
		}
		return nil
	}))

	return func(p Processor) Processor {
		return ProcessWith(func(e *mail.Envelope, task SelectTask) (Result, error) {

			if task == TaskSaveMail {
				var to, body string

				hash := ""
				if len(e.Hashes) > 0 {
					hash = e.Hashes[0]
					e.QueuedId = e.Hashes[0]
				}

				var co *compressor
				// a compressor was set by the Compress processor
				if c, ok := e.Values["zlib-compressor"]; ok {
					body = "gzip"
					co = c.(*compressor)
				}
				// was saved in redis by the Redis processor
				if _, ok := e.Values["redis"]; ok {
					body = "redis"
				}

				for i := range e.RcptTo {

					// use the To header, otherwise rcpt to
					to = trimToLimit(s.fillAddressFromHeader(e, "To"), 255)
					if to == "" {
						// trimToLimit(strings.TrimSpace(e.RcptTo[i].User)+"@"+config.PrimaryHost, 255)
						to = trimToLimit(strings.TrimSpace(e.RcptTo[i].String()), 255)
					}
					mid := trimToLimit(s.fillAddressFromHeader(e, "Message-Id"), 255)
					if mid == "" {
						mid = fmt.Sprintf("%s.%s@%s", hash, e.RcptTo[i].User, config.PrimaryHost)
					}
					// replyTo is the 'Reply-to' header, it may be blank
					replyTo := trimToLimit(s.fillAddressFromHeader(e, "Reply-To"), 255)
					// sender is the 'Sender' header, it may be blank
					sender := trimToLimit(s.fillAddressFromHeader(e, "Sender"), 255)

					recipient := trimToLimit(strings.TrimSpace(e.RcptTo[i].String()), 255)
					contentType := ""
					if v, ok := e.Header["Content-Type"]; ok {
						contentType = trimToLimit(v[0], 255)
					}

					// build the values for the query
					vals = []interface{}{} // clear the vals
					vals = append(vals,
						to,
						trimToLimit(e.MailFrom.String(), 255), // from
						trimToLimit(e.Subject, 255),
						body, // body describes how to interpret the data, eg 'redis' means stored in redis, and 'gzip' stored in mysql, using gzip compression
					)
					// `mail` column
					if body == "redis" {
						// data already saved in redis
						vals = append(vals, "")
					} else if co != nil {
						// use a compressor (automatically adds e.DeliveryHeader)
						vals = append(vals, co.String())

					} else {
						vals = append(vals, e.String())
					}

					vals = append(vals,
						hash, // hash (redis hash if saved in redis)
						contentType,
						recipient,
						s.ip2bint(e.RemoteIP).Bytes(),         // ip_addr store as varbinary(16)
						trimToLimit(e.MailFrom.String(), 255), // return_path
						// is_tls
						e.TLS,
						// message_id
						mid,
						// reply_to
						replyTo,
						sender,
					)

					stmt := s.prepareInsertQuery(1, db)
					err := s.doQuery(1, db, stmt, &vals)
					if err != nil {
						return NewResult(fmt.Sprint("554 Error: could not save email")), StorageError
					}
				}

				// continue to the next Processor in the decorator chain
				return p.Process(e, task)
			} else if task == TaskValidateRcpt {
				// if you need to validate the e.Rcpt then change to:
				if len(e.RcptTo) > 0 {
					// since this is called each time a recipient is added
					// validate only the _last_ recipient that was appended
					last := e.RcptTo[len(e.RcptTo)-1]
					if len(last.User) > 255 {
						// return with an error
						return NewResult(response.Canned.FailRcptCmd), NoSuchUser
					}
				}
				// continue to the next processor
				return p.Process(e, task)
			} else {
				return p.Process(e, task)
			}

		})
	}
}
