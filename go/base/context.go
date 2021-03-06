/*
   Copyright 2016 GitHub Inc.
	 See https://github.com/github/gh-ost/blob/master/LICENSE
*/

package base

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/github/gh-ost/go/mysql"
	"github.com/github/gh-ost/go/sql"

	"gopkg.in/gcfg.v1"
)

// RowsEstimateMethod is the type of row number estimation
type RowsEstimateMethod string

const (
	TableStatusRowsEstimate RowsEstimateMethod = "TableStatusRowsEstimate"
	ExplainRowsEstimate = "ExplainRowsEstimate"
	CountRowsEstimate = "CountRowsEstimate"
)

type CutOver int

const (
	CutOverAtomic CutOver = iota
	CutOverTwoStep = iota
)

var (
	envVariableRegexp = regexp.MustCompile("[$][{](.*)[}]")
)

// MigrationContext has the general, global state of migration. It is used by
// all components throughout the migration process.
type MigrationContext struct {
	DatabaseName                        string
	MigrateDatabaseName                 string
	OriginalTableName                   string
	MigrateTableName                    string
	AlterStatement                      string

	CountTableRows                      bool
	AllowedRunningOnMaster              bool
	AllowedMasterMaster                 bool
	SwitchToRowBinlogFormat             bool
	NullableUniqueKeyAllowed            bool
	ApproveRenamedColumns               bool
	SkipRenamedColumns                  bool

	config                              ContextConfig
	configMutex                         *sync.Mutex
	ConfigFile                          string
	CliUser                             string
	CliPassword                         string
	CliMigrateUser                      string
	CliMigratePassword                  string

	defaultNumRetries                   int64
	ChunkSize                           int64
	niceRatio                           float64
	MaxLagMillisecondsThrottleThreshold int64
	replicationLagQuery                 string
	throttleControlReplicaKeys          *mysql.InstanceKeyMap
	ThrottleFlagFile                    string
	ThrottleAdditionalFlagFile          string
	throttleQuery                       string
	ThrottleCommandedByUser             int64
	maxLoad                             LoadMap
	criticalLoad                        LoadMap
	PostponeCutOverFlagFile             string
	CutOverLockTimeoutSeconds           int64
	PanicFlagFile                       string

	DropServeSocket                     bool
	ServeSocketFile                     string
	ServeTCPPort                        int64

	Noop                                bool
	TestOnReplica                       bool
	Migrate                             bool
	MigrateOnReplica                    bool
	OkToDropTable                       bool
	InitiallyDropOldTable               bool
	InitiallyDropGhostTable             bool
	CutOverType                         CutOver

	TableEngine                         string
	RowsEstimate                        int64
	UsedRowsEstimateMethod              RowsEstimateMethod
	OriginalBinlogFormat                string
	OriginalBinlogRowImage              string
	InspectorConnectionConfig           *mysql.ConnectionConfig
	ApplierConnectionConfig             *mysql.ConnectionConfig
	StartTime                           time.Time
	RowCopyStartTime                    time.Time
	RowCopyEndTime                      time.Time
	LockTablesStartTime                 time.Time
	RenameTablesStartTime               time.Time
	RenameTablesEndTime                 time.Time
	pointOfInterestTime                 time.Time
	pointOfInterestTimeMutex            *sync.Mutex
	CurrentLag                          int64
	TotalRowsCopied                     int64
	TotalDMLEventsApplied               int64
	isThrottled                         bool
	throttleReason                      string
	throttleMutex                       *sync.Mutex
	IsPostponingCutOver                 int64
	CountingRowsFlag                    int64

	OriginalTableColumns                *sql.ColumnList
	OriginalTableUniqueKeys             [](*sql.UniqueKey)
	GhostTableColumns                   *sql.ColumnList
	GhostTableUniqueKeys                [](*sql.UniqueKey)
	UniqueKey                           *sql.UniqueKey
	SharedColumns                       *sql.ColumnList
	ColumnRenameMap                     map[string]string
	MappedSharedColumns                 *sql.ColumnList
	MigrationRangeMinValues             *sql.ColumnValues
	MigrationRangeMaxValues             *sql.ColumnValues
	Iteration                           int64
	MigrationIterationRangeMinValues    *sql.ColumnValues
	MigrationIterationRangeMaxValues    *sql.ColumnValues

	CanStopStreaming                    func() bool
}

type ContextConfig struct {
	Client struct {
		       User     string
		       Password string
	       }
	Osc    struct {
		       Chunk_Size            int64
		       Max_Lag_Millis        int64
		       Replication_Lag_Query string
		       Max_Load              string
	       }
}

var context *MigrationContext

func init() {
	context = newMigrationContext()
}

func newMigrationContext() *MigrationContext {
	return &MigrationContext{
		defaultNumRetries:                   60,
		ChunkSize:                           1000,
		InspectorConnectionConfig:           mysql.NewConnectionConfig(),
		ApplierConnectionConfig:             mysql.NewConnectionConfig(),
		MaxLagMillisecondsThrottleThreshold: 1500,
		CutOverLockTimeoutSeconds:           3,
		maxLoad:                             NewLoadMap(),
		criticalLoad:                        NewLoadMap(),
		throttleMutex:                       &sync.Mutex{},
		throttleControlReplicaKeys:          mysql.NewInstanceKeyMap(),
		configMutex:                         &sync.Mutex{},
		pointOfInterestTimeMutex:            &sync.Mutex{},
		ColumnRenameMap:                     make(map[string]string),
	}
}

// GetMigrationContext
func GetMigrationContext() *MigrationContext {
	return context
}

// GetGhostTableName generates the name of ghost table, based on original table name
func (this *MigrationContext) GetGhostTableName() string {
	if this.Migrate {
		return this.MigrateTableName
	} else {
		return fmt.Sprintf("_%s_gho", this.OriginalTableName)
	}
}

// GetOldTableName generates the name of the "old" table, into which the original table is renamed.
func (this *MigrationContext) GetOldTableName() string {
	if this.TestOnReplica {
		return fmt.Sprintf("_%s_ght", this.OriginalTableName)
	}
	if this.MigrateOnReplica {
		return fmt.Sprintf("_%s_ghr", this.OriginalTableName)
	}
	return fmt.Sprintf("_%s_del", this.OriginalTableName)
}

// GetChangelogTableName generates the name of changelog table, based on original table name
func (this *MigrationContext) GetChangelogTableName() string {
	return fmt.Sprintf("_%s_ghc", this.OriginalTableName)
}

// GetVoluntaryLockName returns a name of a voluntary lock to be used throughout
// the swap-tables process.
func (this *MigrationContext) GetVoluntaryLockName() string {
	return fmt.Sprintf("%s.%s.lock", this.DatabaseName, this.OriginalTableName)
}

// RequiresBinlogFormatChange is `true` when the original binlog format isn't `ROW`
func (this *MigrationContext) RequiresBinlogFormatChange() bool {
	return this.OriginalBinlogFormat != "ROW"
}

// InspectorIsAlsoApplier is `true` when the both inspector and applier are the
// same database instance. This would be true when running directly on master or when
// testing on replica.
func (this *MigrationContext) InspectorIsAlsoApplier() bool {
	return this.InspectorConnectionConfig.Equals(this.ApplierConnectionConfig)
}

// HasMigrationRange tells us whether there's a range to iterate for copying rows.
// It will be `false` if the table is initially empty
func (this *MigrationContext) HasMigrationRange() bool {
	return this.MigrationRangeMinValues != nil && this.MigrationRangeMaxValues != nil
}

func (this *MigrationContext) SetCutOverLockTimeoutSeconds(timeoutSeconds int64) error {
	if timeoutSeconds < 1 {
		return fmt.Errorf("Minimal timeout is 1sec. Timeout remains at %d", this.CutOverLockTimeoutSeconds)
	}
	if timeoutSeconds > 10 {
		return fmt.Errorf("Maximal timeout is 10sec. Timeout remains at %d", this.CutOverLockTimeoutSeconds)
	}
	this.CutOverLockTimeoutSeconds = timeoutSeconds
	return nil
}

func (this *MigrationContext) SetDefaultNumRetries(retries int64) {
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()
	if retries > 0 {
		this.defaultNumRetries = retries
	}
}

func (this *MigrationContext) MaxRetries() int64 {
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()
	retries := this.defaultNumRetries
	return retries
}

func (this *MigrationContext) IsTransactionalTable() bool {
	switch strings.ToLower(this.TableEngine) {
	case "innodb":
		{
			return true
		}
	case "tokudb":
		{
			return true
		}
	}
	return false
}

// ElapsedTime returns time since very beginning of the process
func (this *MigrationContext) ElapsedTime() time.Duration {
	return time.Now().Sub(this.StartTime)
}

// MarkRowCopyStartTime
func (this *MigrationContext) MarkRowCopyStartTime() {
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()
	this.RowCopyStartTime = time.Now()
}

// ElapsedRowCopyTime returns time since starting to copy chunks of rows
func (this *MigrationContext) ElapsedRowCopyTime() time.Duration {
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()

	if this.RowCopyStartTime.IsZero() {
		// Row copy hasn't started yet
		return 0
	}

	if this.RowCopyEndTime.IsZero() {
		return time.Now().Sub(this.RowCopyStartTime)
	}
	return this.RowCopyEndTime.Sub(this.RowCopyStartTime)
}

// ElapsedRowCopyTime returns time since starting to copy chunks of rows
func (this *MigrationContext) MarkRowCopyEndTime() {
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()
	this.RowCopyEndTime = time.Now()
}

// GetTotalRowsCopied returns the accurate number of rows being copied (affected)
// This is not exactly the same as the rows being iterated via chunks, but potentially close enough
func (this *MigrationContext) GetTotalRowsCopied() int64 {
	return atomic.LoadInt64(&this.TotalRowsCopied)
}

func (this *MigrationContext) GetIteration() int64 {
	return atomic.LoadInt64(&this.Iteration)
}

func (this *MigrationContext) MarkPointOfInterest() int64 {
	this.pointOfInterestTimeMutex.Lock()
	defer this.pointOfInterestTimeMutex.Unlock()

	this.pointOfInterestTime = time.Now()
	return atomic.LoadInt64(&this.Iteration)
}

func (this *MigrationContext) TimeSincePointOfInterest() time.Duration {
	this.pointOfInterestTimeMutex.Lock()
	defer this.pointOfInterestTimeMutex.Unlock()

	return time.Now().Sub(this.pointOfInterestTime)
}

func (this *MigrationContext) SetMaxLagMillisecondsThrottleThreshold(maxLagMillisecondsThrottleThreshold int64) {
	if maxLagMillisecondsThrottleThreshold < 1000 {
		maxLagMillisecondsThrottleThreshold = 1000
	}
	atomic.StoreInt64(&this.MaxLagMillisecondsThrottleThreshold, maxLagMillisecondsThrottleThreshold)
}

func (this *MigrationContext) SetChunkSize(chunkSize int64) {
	if chunkSize < 100 {
		chunkSize = 100
	}
	if chunkSize > 100000 {
		chunkSize = 100000
	}
	atomic.StoreInt64(&this.ChunkSize, chunkSize)
}

func (this *MigrationContext) SetThrottled(throttle bool, reason string) {
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()
	this.isThrottled = throttle
	this.throttleReason = reason
}

func (this *MigrationContext) IsThrottled() (bool, string) {
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()
	return this.isThrottled, this.throttleReason
}

func (this *MigrationContext) GetReplicationLagQuery() string {
	var query string

	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()

	query = this.replicationLagQuery
	return query
}

func (this *MigrationContext) SetReplicationLagQuery(newQuery string) {
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()

	this.replicationLagQuery = newQuery
}

func (this *MigrationContext) GetThrottleQuery() string {
	var query string

	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()

	query = this.throttleQuery
	return query
}

func (this *MigrationContext) SetThrottleQuery(newQuery string) {
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()

	this.throttleQuery = newQuery
}

func (this *MigrationContext) GetMaxLoad() LoadMap {
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()

	return this.maxLoad.Duplicate()
}

func (this *MigrationContext) GetCriticalLoad() LoadMap {
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()

	return this.criticalLoad.Duplicate()
}

func (this *MigrationContext) GetNiceRatio() float64 {
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()

	return this.niceRatio
}

func (this *MigrationContext) SetNiceRatio(newRatio float64) {
	if newRatio < 0.0 {
		newRatio = 0.0
	}
	if newRatio > 100.0 {
		newRatio = 100.0
	}

	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()
	this.niceRatio = newRatio
}

// ReadMaxLoad parses the `--max-load` flag, which is in multiple key-value format,
// such as: 'Threads_running=100,Threads_connected=500'
// It only applies changes in case there's no parsing error.
func (this *MigrationContext) ReadMaxLoad(maxLoadList string) error {
	loadMap, err := ParseLoadMap(maxLoadList)
	if err != nil {
		return err
	}
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()

	this.maxLoad = loadMap
	return nil
}

// ReadMaxLoad parses the `--max-load` flag, which is in multiple key-value format,
// such as: 'Threads_running=100,Threads_connected=500'
// It only applies changes in case there's no parsing error.
func (this *MigrationContext) ReadCriticalLoad(criticalLoadList string) error {
	loadMap, err := ParseLoadMap(criticalLoadList)
	if err != nil {
		return err
	}
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()

	this.criticalLoad = loadMap
	return nil
}

func (this *MigrationContext) GetThrottleControlReplicaKeys() *mysql.InstanceKeyMap {
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()

	keys := mysql.NewInstanceKeyMap()
	keys.AddKeys(this.throttleControlReplicaKeys.GetInstanceKeys())
	return keys
}

func (this *MigrationContext) ReadThrottleControlReplicaKeys(throttleControlReplicas string) error {
	keys := mysql.NewInstanceKeyMap()
	if err := keys.ReadCommaDelimitedList(throttleControlReplicas); err != nil {
		return err
	}

	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()

	this.throttleControlReplicaKeys = keys
	return nil
}

func (this *MigrationContext) AddThrottleControlReplicaKey(key mysql.InstanceKey) error {
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()

	this.throttleControlReplicaKeys.AddKey(key)
	return nil
}

// ApplyCredentials sorts out the credentials between the config file and the CLI flags
func (this *MigrationContext) ApplyCredentials() {
	this.configMutex.Lock()
	defer this.configMutex.Unlock()

	if this.config.Client.User != "" {
		this.InspectorConnectionConfig.User = this.config.Client.User
	}
	if this.CliUser != "" {
		// Override
		this.InspectorConnectionConfig.User = this.CliUser
	}
	if this.config.Client.Password != "" {
		this.InspectorConnectionConfig.Password = this.config.Client.Password
	}
	if this.CliPassword != "" {
		// Override
		this.InspectorConnectionConfig.Password = this.CliPassword
	}
}

// ReadConfigFile attempts to read the config file, if it exists
func (this *MigrationContext) ReadConfigFile() error {
	this.configMutex.Lock()
	defer this.configMutex.Unlock()

	if this.ConfigFile == "" {
		return nil
	}
	gcfg.RelaxedParserMode = true
	if err := gcfg.ReadFileInto(&this.config, this.ConfigFile); err != nil {
		return err
	}

	// We accept user & password in the form "${SOME_ENV_VARIABLE}" in which case we pull
	// the given variable from os env
	if submatch := envVariableRegexp.FindStringSubmatch(this.config.Client.User); len(submatch) > 1 {
		this.config.Client.User = os.Getenv(submatch[1])
	}
	if submatch := envVariableRegexp.FindStringSubmatch(this.config.Client.Password); len(submatch) > 1 {
		this.config.Client.Password = os.Getenv(submatch[1])
	}

	return nil
}
