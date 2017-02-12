package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/howeyc/gopass"
	"github.com/kintone/go-kintone"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
	"io"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Configure struct {
	login             string
	password          string
	basicAuthUser     string
	basicAuthPassword string
	apiToken          string
	domain            string
	basic             string
	format            string
	query             string
	appId             uint64
	fields            []string
	filePath          string
	deleteAll         bool
	encoding          string
	guestSpaceId      uint64
	fileDir           string
	accessKey         string
	secretAccessKey   string
	region            string
	bucketName        string
}

var config Configure

const IMPORT_ROW_LIMIT = 100
const EXPORT_ROW_LIMIT = 500

type Column struct {
	Code       string
	Type       string
	IsSubField bool
	Table      string
}

type Columns []*Column

func (p Columns) Len() int {
	return len(p)
}

func (p Columns) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}

func (p Columns) Less(i, j int) bool {
	p1 := p[i]
	code1 := p1.Code
	if p1.IsSubField {
		code1 = p1.Table
	}
	p2 := p[j]
	code2 := p2.Code
	if p2.IsSubField {
		code2 = p2.Table
	}
	if code1 == code2 {
		return p[i].Code < p[j].Code
	}
	return code1 < code2
}

func getFields(app *kintone.App) (map[string]*kintone.FieldInfo, error) {
	fields, err := app.Fields()
	if err != nil {
		return nil, err
	}
	return fields, nil
}

// set column information from fieldinfo
func getColumn(code string, fields map[string]*kintone.FieldInfo) *Column {
	// initialize values
	column := Column{Code: code, IsSubField: false, Table: ""}

	if code == "$id" {
		column.Type = kintone.FT_ID
		return &column
	} else if code == "$revision" {
		column.Type = kintone.FT_REVISION
		return &column
	} else {
		// is this code the one of sub field?
		for _, val := range fields {
			if val.Code == code {
				column.Type = val.Type
				return &column
			}
			if val.Type == kintone.FT_SUBTABLE {
				for _, subField := range val.Fields {
					if subField.Code == code {
						column.IsSubField = true
						column.Type = subField.Type
						column.Table = val.Code
						return &column
					}
				}
			}
		}
	}

	// the code is not found
	column.Type = "UNKNOWN"
	return &column
}

func getEncoding() encoding.Encoding {
	switch config.encoding {
	case "utf-16":
		return unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM)
	case "utf-16be-with-signature":
		return unicode.UTF16(unicode.BigEndian, unicode.ExpectBOM)
	case "utf-16le-with-signature":
		return unicode.UTF16(unicode.LittleEndian, unicode.ExpectBOM)
	case "euc-jp":
		return japanese.EUCJP
	case "sjis":
		return japanese.ShiftJIS
	default:
		return nil
	}
}

func main() {
	var colNames string

	flag.StringVar(&config.login, "u", "", "Login name")
	flag.StringVar(&config.password, "p", "", "Password")
	flag.StringVar(&config.basicAuthUser, "U", "", "Basic authentication user name")
	flag.StringVar(&config.basicAuthPassword, "P", "", "Basic authentication password")
	flag.StringVar(&config.domain, "d", "", "Domain name")
	flag.StringVar(&config.apiToken, "t", "", "API token")
	flag.Uint64Var(&config.appId, "a", 0, "App ID")
	flag.Uint64Var(&config.guestSpaceId, "g", 0, "Guest Space ID")
	flag.StringVar(&config.format, "o", "csv", "Output format: 'json' or 'csv'(default)")
	flag.StringVar(&config.query, "q", "", "Query string")
	flag.StringVar(&colNames, "c", "", "Field names (comma separated)")
	flag.StringVar(&config.filePath, "f", "", "Input file path")
	flag.BoolVar(&config.deleteAll, "D", false, "Delete all records before insert")
	flag.StringVar(&config.encoding, "e", "utf-8", "Character encoding: 'utf-8'(default), 'utf-16', 'utf-16be-with-signature', 'utf-16le-with-signature', 'sjis' or 'euc-jp'")
	flag.StringVar(&config.fileDir, "b", "", "Attachment file directory")

	flag.Parse()

	config.accessKey = os.Getenv("KINTONE_TO_S3_ACCESSKEY")
	config.secretAccessKey = os.Getenv("KINTONE_TO_S3_SECRET")
	config.region = os.Getenv("KINTONE_TO_S3_REGION")
	config.bucketName = os.Getenv("KINTONE_TO_S3_BUCKETNAME")

	config.domain = os.Getenv("KINTONE_DOMAIN")
	config.apiToken = os.Getenv("KINTONE_API_TOKEN")
	appId, _ := strconv.ParseUint(os.Getenv("KINTONE_APP_ID"), 10, 64)
	config.appId = appId

	if config.appId == 0 || (config.apiToken == "" && (config.domain == "" || config.login == "")) {
		flag.PrintDefaults()
		return
	}

	if !strings.Contains(config.domain, ".") {
		config.domain += ".cybozu.com"
	}

	if colNames != "" {
		config.fields = strings.Split(colNames, ",")
		for i, field := range config.fields {
			config.fields[i] = strings.TrimSpace(field)
		}
	}

	var app *kintone.App

	if config.basicAuthUser != "" && config.basicAuthPassword == "" {
		fmt.Printf("Basic authentication password: ")
		pass, _ := gopass.GetPasswd()
		config.basicAuthPassword = string(pass)
	}

	if config.apiToken == "" {
		if config.password == "" {
			fmt.Printf("Password: ")
			pass, _ := gopass.GetPasswd()
			config.password = string(pass)
		}

		app = &kintone.App{
			Domain:       config.domain,
			User:         config.login,
			Password:     config.password,
			AppId:        config.appId,
			GuestSpaceId: config.guestSpaceId,
		}
	} else {
		app = &kintone.App{
			Domain:       config.domain,
			ApiToken:     config.apiToken,
			AppId:        config.appId,
			GuestSpaceId: config.guestSpaceId,
		}
	}

	if config.basicAuthUser != "" {
		app.SetBasicAuth(config.basicAuthUser, config.basicAuthPassword)
	}

	var b bytes.Buffer
	writer := bufio.NewWriter(&b)

	var err error
	err = writeCsv(app, writer)
	//if config.filePath == "" {
	//	if config.format == "json" {
	//		err = writeJson(app, os.Stdout)
	//	} else {
	//		err = writeCsv(app, os.Stdout)
	//	}
	//} else {
	//	var file *os.File
	//	file, err = os.Open(config.filePath)
	//	if err == nil {
	//		defer file.Close()
	//		err = readCsv(app, file)
	//	}
	//}
	if err != nil {
		log.Fatal(err)
	}

	writer.Flush()

	// S3へのアップロード
	sess, err := session.NewSession()
	svc := s3.New(sess, &aws.Config{
		Credentials: credentials.NewStaticCredentials(config.accessKey, config.secretAccessKey, ""),
		Region:      aws.String(config.region),
	})
	_, err = svc.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(config.bucketName),
		Key:    aws.String("golang-kintone-to-s3.csv"),
		ACL:    aws.String("public-read"),
		Body:   bytes.NewReader(b.Bytes()),
	})
	if err != nil {
		log.Println(err.Error())
	}

}

func getRecords(app *kintone.App, fields []string, offset int64) ([]*kintone.Record, bool, error) {

	r := regexp.MustCompile(`limit\s+\d+`)
	if r.MatchString(config.query) {
		records, err := app.GetRecords(fields, config.query)

		if err != nil {
			return nil, true, err
		}
		return records, true, nil
	} else {
		newQuery := config.query + fmt.Sprintf(" limit %v offset %v", EXPORT_ROW_LIMIT, offset)
		records, err := app.GetRecords(fields, newQuery)

		if err != nil {
			return nil, true, err
		}
		return records, len(records) < EXPORT_ROW_LIMIT, nil
	}
}

func getWriter(writer io.Writer) io.Writer {
	encoding := getEncoding()
	if encoding == nil {
		return writer
	}
	return transform.NewWriter(writer, encoding.NewEncoder())
}

func writeJson(app *kintone.App, _writer io.Writer) error {
	i := 0
	offset := int64(0)
	writer := getWriter(_writer)

	fmt.Fprint(writer, "{\"records\": [\n")
	for ; ; offset += EXPORT_ROW_LIMIT {
		records, eof, err := getRecords(app, config.fields, offset)
		if err != nil {
			return err
		}
		for _, record := range records {
			if i > 0 {
				fmt.Fprint(writer, ",\n")
			}
			jsonArray, _ := record.MarshalJSON()
			json := string(jsonArray)
			fmt.Fprint(writer, json)
			i += 1
		}
		if eof {
			break
		}
	}
	fmt.Fprint(writer, "\n]}")

	return nil
}

func makeColumns(fields map[string]*kintone.FieldInfo) Columns {
	columns := make([]*Column, 0)

	var column *Column

	column = &Column{Code: "$id", Type: kintone.FT_ID}
	columns = append(columns, column)
	column = &Column{Code: "$revision", Type: kintone.FT_REVISION}
	columns = append(columns, column)

	for _, val := range fields {
		if val.Code == "" {
			continue
		}
		if val.Type == kintone.FT_SUBTABLE {
			// record id for subtable
			column := &Column{Code: val.Code, Type: val.Type}
			columns = append(columns, column)

			for _, subField := range val.Fields {
				column := &Column{Code: subField.Code, Type: subField.Type, IsSubField: true, Table: val.Code}
				columns = append(columns, column)
			}
		} else {
			column := &Column{Code: val.Code, Type: val.Type}
			columns = append(columns, column)
		}
	}

	return columns
}

func makePartialColumns(fields map[string]*kintone.FieldInfo, partialFields []string) Columns {
	columns := make([]*Column, 0)

	for _, val := range partialFields {
		column := getColumn(val, fields)

		if column.Type == "UNKNOWN" || column.IsSubField {
			continue
		}
		if column.Type == kintone.FT_SUBTABLE {
			// record id for subtable
			column := &Column{Code: column.Code, Type: column.Type}
			columns = append(columns, column)

			// append all sub fields
			field := fields[val]

			for _, subField := range field.Fields {
				column := &Column{Code: subField.Code, Type: subField.Type, IsSubField: true, Table: val}
				columns = append(columns, column)
			}
		} else {
			columns = append(columns, column)
		}
	}
	return columns
}

func getSubTableRowCount(record *kintone.Record, columns []*Column) int {
	var ret = 1
	for _, c := range columns {
		if c.IsSubField {
			subTable := record.Fields[c.Table].(kintone.SubTableField)

			count := len(subTable)
			if count > ret {
				ret = count
			}
		}
	}

	return ret
}

func hasSubTable(columns []*Column) bool {
	for _, c := range columns {
		if c.IsSubField {
			return true
		}
	}
	return false
}

func writeCsv(app *kintone.App, _writer io.Writer) error {
	i := uint64(0)
	offset := int64(0)
	writer := getWriter(_writer)
	var columns Columns

	// retrieve field list
	fields, err := getFields(app)
	if err != nil {
		return err
	}

	hasTable := false
	for ; ; offset += EXPORT_ROW_LIMIT {
		records, eof, err := getRecords(app, config.fields, offset)
		if err != nil {
			return err
		}

		for _, record := range records {
			if i == 0 {
				// write csv header
				if config.fields == nil {
					columns = makeColumns(fields)
				} else {
					columns = makePartialColumns(fields, config.fields)
				}
				//sort.Sort(columns)
				j := 0
				hasTable = hasSubTable(columns)
				if hasTable {
					fmt.Fprint(writer, "*")
					j++
				}
				for _, f := range columns {
					if j > 0 {
						fmt.Fprint(writer, ",")
					}
					fmt.Fprint(writer, "\""+f.Code+"\"")
					j++
				}
				fmt.Fprint(writer, "\r\n")
			}
			rowId := record.Id()
			if rowId == 0 {
				rowId = i
			}

			// determine subtable's row count
			rowNum := getSubTableRowCount(record, columns)

			for j := 0; j < rowNum; j++ {
				k := 0
				if hasTable {
					if j == 0 {
						fmt.Fprint(writer, "*")
					}
					k++
				}

				for _, f := range columns {
					if k > 0 {
						fmt.Fprint(writer, ",")
					}

					if f.Code == "$id" {
						fmt.Fprintf(writer, "\"%d\"", record.Id())
					} else if f.Code == "$revision" {
						fmt.Fprintf(writer, "\"%d\"", record.Revision())
					} else if f.Type == kintone.FT_SUBTABLE {
						table := record.Fields[f.Code].(kintone.SubTableField)
						if j < len(table) {
							fmt.Fprintf(writer, "\"%d\"", table[j].Id())
						}
					} else if f.IsSubField {
						table := record.Fields[f.Table].(kintone.SubTableField)
						if j < len(table) {
							subField := table[j].Fields[f.Code]
							if f.Type == kintone.FT_FILE {
								dir := fmt.Sprintf("%s-%d-%d", f.Code, rowId, j)
								err := downloadFile(app, subField, dir)
								if err != nil {
									return err
								}
							}
							fmt.Fprint(writer, "\""+escapeCol(toString(subField, "\n"))+"\"")
						}
					} else {
						field := record.Fields[f.Code]
						if field != nil {
							if j == 0 && f.Type == kintone.FT_FILE {
								dir := fmt.Sprintf("%s-%d", f.Code, rowId)
								err := downloadFile(app, field, dir)
								if err != nil {
									return err
								}
							}
							fmt.Fprint(writer, "\""+escapeCol(toString(field, "\n"))+"\"")
						}
					}
					k++
				}
				fmt.Fprint(writer, "\r\n")
			}
			i++
		}
		if eof {
			break
		}
	}

	return nil
}

func downloadFile(app *kintone.App, field interface{}, dir string) error {
	if config.fileDir == "" {
		return nil
	}

	v, ok := field.(kintone.FileField)
	if !ok {
		return nil
	}

	if len(v) == 0 {
		return nil
	}

	fileDir := fmt.Sprintf("%s%c%s", config.fileDir, os.PathSeparator, dir)
	if err := os.MkdirAll(fileDir, 0777); err != nil {
		return err
	}

	for idx, file := range v {
		path := fmt.Sprintf("%s%c%s", fileDir, os.PathSeparator, file.Name)
		data, err := app.Download(file.FileKey)
		if err != nil {
			return err
		}

		fo, err := os.Create(path)
		if err != nil {
			return err
		}
		defer fo.Close()

		// make a buffer to keep chunks that are read
		buf := make([]byte, 256*1024)
		for {
			// read a chunk
			n, err := data.Reader.Read(buf)
			if err != nil && err != io.EOF {
				return err
			}
			if n == 0 {
				break
			}

			// write a chunk
			if _, err := fo.Write(buf[:n]); err != nil {
				return err
			}
		}

		v[idx].Name = fmt.Sprintf("%s%c%s", dir, os.PathSeparator, file.Name)
	}

	return nil
}

func escapeCol(s string) string {
	return strings.Replace(s, "\"", "\"\"", -1)
}

func getType(f interface{}) string {
	switch f.(type) {
	case kintone.SingleLineTextField:
		return kintone.FT_SINGLE_LINE_TEXT
	case kintone.MultiLineTextField:
		return kintone.FT_MULTI_LINE_TEXT
	case kintone.RichTextField:
		return kintone.FT_RICH_TEXT
	case kintone.DecimalField:
		return kintone.FT_DECIMAL
	case kintone.CalcField:
		return kintone.FT_CALC
	case kintone.CheckBoxField:
		return kintone.FT_CHECK_BOX
	case kintone.RadioButtonField:
		return kintone.FT_RADIO
	case kintone.SingleSelectField:
		return kintone.FT_SINGLE_SELECT
	case kintone.MultiSelectField:
		return kintone.FT_MULTI_SELECT
	case kintone.FileField:
		return kintone.FT_FILE
	case kintone.LinkField:
		return kintone.FT_LINK
	case kintone.DateField:
		return kintone.FT_DATE
	case kintone.TimeField:
		return kintone.FT_TIME
	case kintone.DateTimeField:
		return kintone.FT_DATETIME
	case kintone.UserField:
		return kintone.FT_USER
	case kintone.OrganizationField:
		return kintone.FT_ORGANIZATION
	case kintone.GroupField:
		return kintone.FT_GROUP
	case kintone.CategoryField:
		return kintone.FT_CATEGORY
	case kintone.StatusField:
		return kintone.FT_STATUS
	case kintone.RecordNumberField:
		return kintone.FT_RECNUM
	case kintone.AssigneeField:
		return kintone.FT_ASSIGNEE
	case kintone.CreatorField:
		return kintone.FT_CREATOR
	case kintone.ModifierField:
		return kintone.FT_MODIFIER
	case kintone.CreationTimeField:
		return kintone.FT_CTIME
	case kintone.ModificationTimeField:
		return kintone.FT_MTIME
	case kintone.SubTableField:
		return kintone.FT_SUBTABLE
	}
	return ""
}

func toString(f interface{}, delimiter string) string {

	if delimiter == "" {
		delimiter = ","
	}
	switch f.(type) {
	case kintone.SingleLineTextField:
		singleLineTextField := f.(kintone.SingleLineTextField)
		return string(singleLineTextField)
	case kintone.MultiLineTextField:
		multiLineTextField := f.(kintone.MultiLineTextField)
		return string(multiLineTextField)
	case kintone.RichTextField:
		richTextField := f.(kintone.RichTextField)
		return string(richTextField)
	case kintone.DecimalField:
		decimalField := f.(kintone.DecimalField)
		return string(decimalField)
	case kintone.CalcField:
		calcField := f.(kintone.CalcField)
		return string(calcField)
	case kintone.RadioButtonField:
		radioButtonField := f.(kintone.RadioButtonField)
		return string(radioButtonField)
	case kintone.LinkField:
		linkField := f.(kintone.LinkField)
		return string(linkField)
	case kintone.StatusField:
		statusField := f.(kintone.StatusField)
		return string(statusField)
	case kintone.RecordNumberField:
		recordNumberField := f.(kintone.RecordNumberField)
		return string(recordNumberField)
	case kintone.CheckBoxField:
		checkBoxField := f.(kintone.CheckBoxField)
		return strings.Join(checkBoxField, delimiter)
	case kintone.MultiSelectField:
		multiSelectField := f.(kintone.MultiSelectField)
		return strings.Join(multiSelectField, delimiter)
	case kintone.CategoryField:
		categoryField := f.(kintone.CategoryField)
		return strings.Join(categoryField, delimiter)
	case kintone.SingleSelectField:
		singleSelect := f.(kintone.SingleSelectField)
		return singleSelect.String
	case kintone.FileField:
		fileField := f.(kintone.FileField)
		files := make([]string, 0, len(fileField))
		for _, file := range fileField {
			files = append(files, file.Name)
		}
		return strings.Join(files, delimiter)
	case kintone.DateField:
		dateField := f.(kintone.DateField)
		if dateField.Valid {
			return dateField.Date.Format("2006-01-02")
		} else {
			return ""
		}
	case kintone.TimeField:
		timeField := f.(kintone.TimeField)
		if timeField.Valid {
			return timeField.Time.Format("15:04:05")
		} else {
			return ""
		}
	case kintone.DateTimeField:
		dateTimeField := f.(kintone.DateTimeField)
		if dateTimeField.Valid {
			return dateTimeField.Time.Format(time.RFC3339)
		} else {
			return ""
		}
	case kintone.UserField:
		userField := f.(kintone.UserField)
		users := make([]string, 0, len(userField))
		for _, user := range userField {
			users = append(users, user.Code)
		}
		return strings.Join(users, delimiter)
	case kintone.OrganizationField:
		organizationField := f.(kintone.OrganizationField)
		organizations := make([]string, 0, len(organizationField))
		for _, organization := range organizationField {
			organizations = append(organizations, organization.Code)
		}
		return strings.Join(organizations, delimiter)
	case kintone.GroupField:
		groupField := f.(kintone.GroupField)
		groups := make([]string, 0, len(groupField))
		for _, group := range groupField {
			groups = append(groups, group.Code)
		}
		return strings.Join(groups, delimiter)
	case kintone.AssigneeField:
		assigneeField := f.(kintone.AssigneeField)
		users := make([]string, 0, len(assigneeField))
		for _, user := range assigneeField {
			users = append(users, user.Code)
		}
		return strings.Join(users, delimiter)
	case kintone.CreatorField:
		creatorField := f.(kintone.CreatorField)
		return creatorField.Code
	case kintone.ModifierField:
		modifierField := f.(kintone.ModifierField)
		return modifierField.Code
	case kintone.CreationTimeField:
		creationTimeField := f.(kintone.CreationTimeField)
		return time.Time(creationTimeField).Format(time.RFC3339)
	case kintone.ModificationTimeField:
		modificationTimeField := f.(kintone.ModificationTimeField)
		return time.Time(modificationTimeField).Format(time.RFC3339)
	case kintone.SubTableField:
		return "" // unsupported
	}
	return ""
}
