// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/DataDog/datadog-agent/comp/core/autodiscovery/integration"
	"github.com/DataDog/datadog-agent/comp/logs/agent/config"
	integrations "github.com/DataDog/datadog-agent/comp/logs/integrations/def"
	integrationsmock "github.com/DataDog/datadog-agent/comp/logs/integrations/mock"
	pkgconfigsetup "github.com/DataDog/datadog-agent/pkg/config/setup"
	"github.com/DataDog/datadog-agent/pkg/logs/internal/util"
	"github.com/DataDog/datadog-agent/pkg/logs/message"
	"github.com/DataDog/datadog-agent/pkg/logs/pipeline"
	"github.com/DataDog/datadog-agent/pkg/logs/pipeline/mock"
	"github.com/DataDog/datadog-agent/pkg/logs/sources"
	"github.com/DataDog/datadog-agent/pkg/logs/status"
)

type LauncherTestSuite struct {
	suite.Suite
	testDir  string
	testPath string

	outputChan       chan *message.Message
	pipelineProvider pipeline.Provider
	source           *sources.LogSource
	integrationsComp integrations.Component
	s                *Launcher
}

func (suite *LauncherTestSuite) SetupTest() {
	suite.pipelineProvider = mock.NewMockProvider()
	suite.outputChan = suite.pipelineProvider.NextPipelineChan()
	suite.integrationsComp = integrationsmock.Mock()
	suite.testDir = suite.T().TempDir()
	suite.testPath = filepath.Join(suite.testDir, "logs_integration_test.log")

	suite.source = sources.NewLogSource(suite.T().Name(), &config.LogsConfig{Type: config.IntegrationType, Path: suite.testPath})
	// Override `logs_config.run_path` before calling `sources.NewLogSources()` as otherwise
	// it will try and create `/opt/datadog` directory and fail
	pkgconfigsetup.Datadog().SetWithoutSource("logs_config.run_path", suite.testDir)

	suite.s = NewLauncher(sources.NewLogSources(), suite.integrationsComp)
	suite.s.fileSizeMax = 10 * 1024 * 1024
	status.InitStatus(pkgconfigsetup.Datadog(), util.CreateSources([]*sources.LogSource{suite.source}))
	suite.s.runPath = suite.testDir
}

func (suite *LauncherTestSuite) TestFileCreation() {
	id := "123456789"
	source := sources.NewLogSource("testLogsSource", &config.LogsConfig{Type: config.IntegrationType, Identifier: "123456789", Path: suite.testPath})
	sources.NewLogSources().AddSource(source)

	logFilePath, err := suite.s.createFile(id)
	assert.NoError(suite.T(), err)
	assert.NotNil(suite.T(), logFilePath)
}

func (suite *LauncherTestSuite) TestSendLog() {

	mockConf := &integration.Config{}
	mockConf.Provider = "container"
	mockConf.LogsConfig = integration.Data(`[{"type": "integration", "source": "foo", "service": "bar"}]`)

	filepathChan := make(chan string)
	fileLogChan := make(chan string)
	suite.s.writeLogToFileFunction = func(logFilePath, log string) error {
		fileLogChan <- log
		filepathChan <- logFilePath
		return nil
	}

	id := "123456789"

	suite.s.Start(nil, nil, nil, nil)
	suite.integrationsComp.RegisterIntegration(id, *mockConf)

	logSample := "hello world"
	suite.integrationsComp.SendLog(logSample, id)

	foundSource := suite.s.sources.GetSources()[0]
	assert.Equal(suite.T(), foundSource.Config.Type, config.FileType)
	assert.Equal(suite.T(), foundSource.Config.Source, "foo")
	assert.Equal(suite.T(), foundSource.Config.Service, "bar")
	expectedPath := filepath.Join(suite.s.runPath, suite.s.integrationToFile[id].filename)

	assert.Equal(suite.T(), logSample, <-fileLogChan)
	assert.Equal(suite.T(), expectedPath, <-filepathChan)
}

func (suite *LauncherTestSuite) TestWriteLogToFile() {
	logText := "hello world"
	err := suite.s.writeLogToFileFunction(suite.testPath, logText)
	require.Nil(suite.T(), err)

	fileContents, err := os.ReadFile(suite.testPath)

	assert.NoError(suite.T(), err)
	assert.Equal(suite.T(), logText+"\n", string(fileContents))
}

func (suite *LauncherTestSuite) TestWriteMultipleLogsToFile() {
	var err error
	err = suite.s.writeLogToFileFunction(suite.testPath, "line 1")
	require.Nil(suite.T(), err, "error writing line 1")

	err = suite.s.writeLogToFileFunction(suite.testPath, "line 2")
	require.Nil(suite.T(), err, "error writing line 2")

	err = suite.s.writeLogToFileFunction(suite.testPath, "line 3")
	require.Nil(suite.T(), err, "error writing line 3")

	fileContents, err := os.ReadFile(suite.testPath)

	assert.NoError(suite.T(), err)
	expectedContent := "line 1\nline 2\nline 3\n"
	assert.Equal(suite.T(), expectedContent, string(fileContents))
}

// TestDeleteFile tests that deleteFile properly deletes the correct file
func (suite *LauncherTestSuite) TestDeleteFile() {
	filename := "testfile.log"
	filepath := filepath.Join(suite.s.runPath, filename)
	file, err := os.Create(filepath)
	fileinfo := &fileInfo{filename: filename, size: int64(0)}
	assert.Nil(suite.T(), err)

	info, err := os.Stat(filepath)
	assert.Nil(suite.T(), err)
	assert.Equal(suite.T(), int64(0), info.Size(), "Newly created file size not zero")

	// Write data the file and make sure ensureFileSize deletes the file for being too large
	data := make([]byte, 2*1024*1024)
	file.Write(data)
	file.Close()

	info, err = os.Stat(filepath)
	assert.Nil(suite.T(), err)
	assert.Equal(suite.T(), int64(2*1024*1024), info.Size())

	err = suite.s.deleteFile(fileinfo)
	assert.Nil(suite.T(), err)

	_, err = os.Stat(filepath)
	assert.True(suite.T(), os.IsNotExist(err))
}

// TestIntegrationLogFilePath ensures the filepath for the logs files are correct
func (suite *LauncherTestSuite) TestIntegrationLogFilePath() {
	id := "123456789"
	actualFilePath := suite.s.integrationLogFilePath(id)
	expectedFilePath := filepath.Join(suite.s.runPath, id+".log")
	assert.Equal(suite.T(), expectedFilePath, actualFilePath)

	id = "1234 5678:myIntegration"
	actualFilePath = suite.s.integrationLogFilePath(id)
	expectedFilePath = filepath.Join(suite.s.runPath, "1234 5678_myIntegration.log")
	assert.Equal(suite.T(), expectedFilePath, actualFilePath)
}

// TestFileNameToID ensures file names are decoded to their proper id
func (suite *LauncherTestSuite) TestFileNameToID() {
	tests := []struct {
		input    string
		expected string
	}{
		{"file_name_1234.log", "file_name:1234"},
		{"example_test_5678abcd.log", "example_test:5678abcd"},
		{"integration with spaces_5678.log", "integration with spaces:5678"},
		{"file_with_multiple_underscores_9999.log", "file_with_multiple_underscores:9999"},
	}

	for _, tt := range tests {
		suite.T().Run(tt.input, func(_ *testing.T) {
			result := fileNameToID(tt.input)
			assert.Equal(suite.T(), tt.expected, result)
		})
	}
}

// TestFileExceedsSingleFileLimit ensures individual files cannot exceed file
// limit sizes
func (suite *LauncherTestSuite) TestFileExceedsSingleFileLimit() {
	oneMB := int64(1 * 1024 * 1024)
	suite.s.combinedUsageMax = 2 * oneMB
	suite.s.fileSizeMax = oneMB

	filename := "sample_integration_123.log"
	filepath := filepath.Join(suite.s.runPath, filename)
	file, err := os.Create(filepath)
	assert.Nil(suite.T(), err)

	file.Write(make([]byte, oneMB))
	file.Close()

	suite.s.Start(nil, nil, nil, nil)

	integrationLog := integrations.IntegrationLog{
		Log:           "sample log",
		IntegrationID: "sample_integration:123",
	}

	suite.s.receiveLogs(integrationLog)

	assert.Equal(suite.T(), int64(len(integrationLog.Log)+1), suite.s.combinedUsageSize)
	assert.Equal(suite.T(), int64(len(integrationLog.Log)+1), suite.s.integrationToFile["sample_integration:123"].size)
	assert.Equal(suite.T(), 1, len(suite.s.integrationToFile))
}

// TestScanInitialFiles ensures files already present in the runPath for the
// launcher are detected and managed upon launcher start
func (suite *LauncherTestSuite) TestScanInitialFiles() {
	filename := "sample_integration_123.log"
	fileSize := int64(1 * 1024 * 1024)

	file, err := os.Create(filepath.Join(suite.s.runPath, filename))
	assert.Nil(suite.T(), err)

	data := make([]byte, fileSize)
	file.Write(data)
	file.Close()

	suite.s.scanInitialFiles(suite.s.runPath)
	fileID := fileNameToID(filename)
	actualFileInfo := suite.s.integrationToFile[fileID]

	assert.NotEmpty(suite.T(), suite.s.integrationToFile)
	assert.Equal(suite.T(), actualFileInfo.filename, filename)
	assert.Equal(suite.T(), fileSize, actualFileInfo.size)
	assert.Equal(suite.T(), fileSize, suite.s.combinedUsageSize)
}

// TestCreateFileAfterScanInitialFile ensures files tracked by scanInitialFiles
// are not created again after they've already been scanned
func (suite *LauncherTestSuite) TestCreateFileAfterScanInitialFile() {
	filename := "sample_integration_123.log"
	fileSize := int64(1 * 1024 * 1024)

	file, err := os.Create(filepath.Join(suite.s.runPath, filename))
	assert.Nil(suite.T(), err)

	data := make([]byte, fileSize)
	file.Write(data)
	file.Close()

	suite.s.scanInitialFiles(suite.s.runPath)
	fileID := fileNameToID(filename)
	scannedFile := suite.s.integrationToFile[fileID]

	assert.NotEmpty(suite.T(), suite.s.integrationToFile)
	assert.Equal(suite.T(), filename, scannedFile.filename)
	assert.Equal(suite.T(), fileSize, scannedFile.size)
	assert.Equal(suite.T(), fileSize, suite.s.combinedUsageSize)

	mockConf := &integration.Config{}
	mockConf.Provider = "container"
	mockConf.LogsConfig = integration.Data(`[{"type": "integration", "source": "foo", "service": "bar"}]`)

	filepathChan := make(chan string)
	fileLogChan := make(chan string)
	suite.s.writeLogToFileFunction = func(logFilePath, log string) error {
		fileLogChan <- log
		filepathChan <- logFilePath
		return nil
	}

	suite.s.Start(nil, nil, nil, nil)
	suite.integrationsComp.RegisterIntegration(fileID, *mockConf)
	assert.Equal(suite.T(), 1, len(suite.s.integrationToFile))

	logSample := "hello world"
	suite.integrationsComp.SendLog(logSample, fileID)

	foundSource := suite.s.sources.GetSources()[0]
	assert.Equal(suite.T(), foundSource.Config.Type, config.FileType)
	assert.Equal(suite.T(), foundSource.Config.Source, "foo")
	assert.Equal(suite.T(), foundSource.Config.Service, "bar")

	assert.Equal(suite.T(), logSample, <-fileLogChan)
}

// TestSentLogExceedsTotalUsage ensures files are deleted when a sent log causes a
// disk usage overage
func (suite *LauncherTestSuite) TestSentLogExceedsTotalUsage() {
	suite.s.combinedUsageMax = 3 * 1024 * 1024

	filename1 := "sample_integration1_123.log"
	filename2 := "sample_integration2_123.log"
	filename3 := "sample_integration3_123.log"

	file1, err := os.Create(filepath.Join(suite.s.runPath, filename1))
	assert.Nil(suite.T(), err)
	file2, err := os.Create(filepath.Join(suite.s.runPath, filename2))
	assert.Nil(suite.T(), err)
	file3, err := os.Create(filepath.Join(suite.s.runPath, filename3))
	assert.Nil(suite.T(), err)

	dataOneMB := make([]byte, 1*1024*1024)
	file1.Write(dataOneMB)
	file2.Write(dataOneMB)
	file3.Write(dataOneMB)
	file1.Close()
	file2.Close()
	file3.Close()

	suite.s.Start(nil, nil, nil, nil)

	integrationLog := integrations.IntegrationLog{
		Log:           "sample log",
		IntegrationID: "sample_integration1:123",
	}

	suite.s.receiveLogs(integrationLog)

	file1Stat, err := os.Stat(filepath.Join(suite.s.runPath, filename1))
	assert.Nil(suite.T(), err)
	file2Stat, err := os.Stat(filepath.Join(suite.s.runPath, filename2))
	assert.Nil(suite.T(), err)
	file3Stat, err := os.Stat(filepath.Join(suite.s.runPath, filename3))
	assert.Nil(suite.T(), err)

	actualSize := file1Stat.Size() + file2Stat.Size() + file3Stat.Size()

	assert.Equal(suite.T(), suite.s.combinedUsageSize, actualSize)
	assert.Equal(suite.T(), suite.s.integrationToFile["sample_integration2:123"], suite.s.getLeastRecentlyModifiedFile())
}

// TestInitialLogsExceedTotalUsageMultipleFiles ensures initial files are deleted if they
// exceed total allowed disk space
func (suite *LauncherTestSuite) TestInitialLogsExceedTotalUsageMultipleFiles() {
	oneMB := int64(1 * 1024 * 1024)
	suite.s.combinedUsageMax = oneMB

	filename1 := "sample_integration1_123.log"
	filename2 := "sample_integration2_123.log"

	dataOneMB := make([]byte, oneMB)

	file1, err := os.Create(filepath.Join(suite.s.runPath, filename1))
	assert.Nil(suite.T(), err)
	file2, err := os.Create(filepath.Join(suite.s.runPath, filename2))
	assert.Nil(suite.T(), err)

	file1.Write(dataOneMB)
	file2.Write(dataOneMB)
	file1.Close()
	file2.Close()

	suite.s.Start(nil, nil, nil, nil)

	assert.Equal(suite.T(), oneMB, suite.s.combinedUsageSize)
	assert.Equal(suite.T(), 2, len(suite.s.integrationToFile))
}

// TestInitialLogExceedsTotalUsageSingleFile ensures an initial file won't
// exceed the total allowed disk usage space
func (suite *LauncherTestSuite) TestInitialLogExceedsTotalUsageSingleFile() {
	oneMB := int64(1 * 1024 * 1024)
	suite.s.combinedUsageMax = oneMB

	filename := "sample_integration1_123.log"
	dataTwoMB := make([]byte, 2*oneMB)

	file, err := os.Create(filepath.Join(suite.s.runPath, filename))
	assert.Nil(suite.T(), err)

	file.Write(dataTwoMB)
	file.Close()

	suite.s.Start(nil, nil, nil, nil)

	assert.Equal(suite.T(), int64(0), suite.s.combinedUsageSize)
	assert.Equal(suite.T(), 1, len(suite.s.integrationToFile))
}

// TestScanInitialFilesDeletesProperly ensures the scanInitialFiles function
// properly deletes log files once the sum of sizes for the scanned files is too
// large
func (suite *LauncherTestSuite) TestScanInitialFilesDeletesProperly() {
	err := os.RemoveAll(suite.s.runPath)
	assert.Nil(suite.T(), err)
	os.MkdirAll(suite.s.runPath, 0755)
	assert.Nil(suite.T(), err)

	oneMB := int64(1 * 1024 * 1024)
	suite.s.combinedUsageMax = oneMB

	filename1 := "sample_integration1_123.log"
	filename2 := "sample_integration2_123.log"

	name := filepath.Join(suite.s.runPath, filename1)
	file1, err := os.Create(name)
	assert.Nil(suite.T(), err)
	file2, err := os.Create(filepath.Join(suite.s.runPath, filename2))
	assert.Nil(suite.T(), err)

	dataOneMB := make([]byte, oneMB)
	file1.Write(dataOneMB)
	file2.Write(dataOneMB)
	file1.Close()
	file2.Close()

	suite.s.scanInitialFiles(suite.s.runPath)

	// make sure there is only one file in the directory
	files, err := os.ReadDir(suite.s.runPath)
	assert.Nil(suite.T(), err)

	fileCount := 0
	for _, file := range files {
		if !file.IsDir() {
			fileCount++
		}
	}

	assert.Equal(suite.T(), 1, fileCount)
}

func TestLauncherTestSuite(t *testing.T) {
	suite.Run(t, new(LauncherTestSuite))
}

// TestReadOnlyFileSystem ensures the launcher doesn't panic in a read-only
// file system. There will be errors but it should handle them gracefully.
func TestReadOnlyFileSystem(t *testing.T) {
	readOnlyDir := filepath.Join(t.TempDir(), "readonly")
	err := os.Mkdir(readOnlyDir, 0444)
	assert.Nil(t, err, "Unable to make tempdir readonly")

	pkgconfigsetup.Datadog().SetWithoutSource("logs_config.run_path", readOnlyDir)

	integrationsComp := integrationsmock.Mock()
	s := NewLauncher(sources.NewLogSources(), integrationsComp)

	// Check the launcher doesn't block on receiving channels
	mockConf := &integration.Config{}
	mockConf.Provider = "container"
	mockConf.LogsConfig = integration.Data(`[{"type": "integration", "source": "foo", "service": "bar"}]`)
	id := "123456789"

	s.Start(nil, nil, nil, nil)
	integrationsComp.RegisterIntegration(id, *mockConf)

	logSample := "hello world"
	integrationsComp.SendLog(logSample, id)

	// send a second log to make sure the launcher isn't blocking
	integrationsComp.SendLog(logSample, id)
}
