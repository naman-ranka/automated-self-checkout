// ----------------------------------------------------------------------------------
// Copyright 2023 Intel Corp.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	   http://www.apache.org/licenses/LICENSE-2.0
//
//	Unless required by applicable law or agreed to in writing, software
//	distributed under the License is distributed on an "AS IS" BASIS,
//	WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//	See the License for the specific language governing permissions and
//	limitations under the License.
//
// ----------------------------------------------------------------------------------

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type StartPolicy int

const (
	Ignore StartPolicy = iota
	Exit
	RemoveRestart
)

func (policy StartPolicy) String() string {
	return [...]string{
		"ignore",
		"exit",
		"remove-and-restart",
	}[policy]
}

const (
	ENV_KEY_VALUE_DELIMITER = "="

	scriptDir                = "./scripts"
	envFileDir               = "./envs"
	pipelineProfileEnv       = "PIPELINE_PROFILE"
	resourceDir              = "res"
	pipelineConfigFileName   = "configuration.yaml"
	commandLineArgsDelimiter = " "
	streamDensityScript      = "./stream_density.sh"

	defaultOvmsServerStartWaitTime = time.Duration(10 * time.Second)
)

const (
	OVMS_SERVER_DOCKER_IMG_ENV      = "OVMS_SERVER_IMAGE_TAG"
	OVMS_SERVER_START_UP_MSG_ENV    = "SERVER_START_UP_MSG"
	SERVER_CONTAINER_NAME_ENV       = "SERVER_CONTAINER_NAME"
	OVMS_MODEL_CONFIG_JSON_PATH_ENV = "OVMS_MODEL_CONFIG_JSON"
	OVMS_INIT_WAIT_TIME_ENV         = "SERVER_INIT_WAIT_TIME"
	CID_COUNT_ENV                   = "cid_count"
)

type OvmsServerInfo struct {
	ServerDockerScript       string
	ServerDockerImage        string
	ServerContainerName      string
	ServerConfig             string
	StartupMessage           string
	InitWaitTime             string
	EnvironmentVariableFiles []string
	StartUpPolicy            string
}

type OvmsClientInfo struct {
	PipelineScript           string
	PipelineInputArgs        string
	PipelineStreamDensityRun string
	EnvironmentVariableFiles []string
}
type OvmsClientConfig struct {
	OvmsSingleContainer bool
	OvmsServer          OvmsServerInfo
	OvmsClient          OvmsClientInfo
}

type Flags struct {
	FlagSet   *flag.FlagSet
	configDir string
}

func main() {
	flagSet := flag.NewFlagSet("", flag.ExitOnError)
	flags := &Flags{
		FlagSet: flagSet,
	}
	flagSet.StringVar(&flags.configDir, "configDir", filepath.Join(".", resourceDir), "")
	flagSet.StringVar(&flags.configDir, "cd", filepath.Join(".", resourceDir), "")
	err := flags.FlagSet.Parse(os.Args[1:])
	if err != nil {
		flagSet.Usage()
		log.Fatalln(err)
	}

	// the config yaml file is in res/ folder of the "pipeline profile" directory
	contents, err := flags.readPipelineConfig()
	if err != nil {
		log.Fatalf("failed to read configuration yaml file: %v", err)
	}

	data := make(map[string]any)
	err = yaml.Unmarshal(contents, &data)
	if err != nil {
		log.Fatalf("failed to unmarshal configuration file configuration.yaml: %v", err)
	}

	log.Println("data: ", data)

	// convert to struct
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		log.Fatalf("could not marshal map to JSON: %v", err)
	}

	var ovmsClientConf OvmsClientConfig
	if err := json.Unmarshal(jsonBytes, &ovmsClientConf); err != nil {
		log.Fatalf("could not unmarshal JSON data to %T: %v", ovmsClientConf, err)
	}

	log.Println("successfully converted to OvmsClientConfig struct", ovmsClientConf)

	// if OvmsSingleContainer mode is true, then we don't launcher another ovms server
	// as the client itself has it like C-Api case
	if ovmsClientConf.OvmsSingleContainer {
		log.Println("running in single container mode, no distributed client-server")
	} else {
		// launcher ovms server
		startOvmsServer(ovmsClientConf)
	}

	//launch the pipeline script from the config
	if err := launchPipelineScript(ovmsClientConf); err != nil {
		log.Fatalf("found error while launching pipeline script: %v", err)
	}

}

func startOvmsServer(ovmsClientConf OvmsClientConfig) {
	if len(ovmsClientConf.OvmsServer.ServerDockerScript) == 0 {
		log.Println("Error founding any server launch script from OvmsServer.ServerDockerScript, please check configuration.yaml file")
		os.Exit(1)
	}

	log.Println("OVMS server config to launcher: ", ovmsClientConf.OvmsServer)
	os.Setenv(OVMS_SERVER_START_UP_MSG_ENV, ovmsClientConf.OvmsServer.StartupMessage)
	os.Setenv(SERVER_CONTAINER_NAME_ENV, ovmsClientConf.OvmsServer.ServerContainerName)
	os.Setenv(OVMS_SERVER_DOCKER_IMG_ENV, ovmsClientConf.OvmsServer.ServerDockerImage)
	os.Setenv(OVMS_MODEL_CONFIG_JSON_PATH_ENV, ovmsClientConf.OvmsServer.ServerConfig)

	serverScript := filepath.Join(scriptDir, ovmsClientConf.OvmsServer.ServerDockerScript)
	ovmsSrvLaunch, err := exec.LookPath(serverScript)
	if err != nil {
		log.Printf("Error: failed to get ovms server launch script path: %v", err)
		os.Exit(1)
	}

	log.Println("launch ovms server script:", ovmsSrvLaunch)
	startupPolicy := Ignore.String()
	if len(ovmsClientConf.OvmsServer.StartUpPolicy) > 0 {
		startupPolicy = ovmsClientConf.OvmsServer.StartUpPolicy
	}
	switch startupPolicy {
	case Ignore.String(), Exit.String(), RemoveRestart.String():
		log.Println("chose ovms server startup policy:", startupPolicy)
	default:
		startupPolicy = Ignore.String()
		log.Println("ovms server startup policy defaults to", Ignore.String())
	}

	cmd := exec.Command(ovmsSrvLaunch)
	cmd.Env = os.Environ()
	origEnvs := make([]string, len(cmd.Env))
	copy(origEnvs, cmd.Env)
	// apply all envs from env files if any
	envList := ovmsClientConf.OvmsServer.readServerEnvs(envFileDir)
	cmd.Env = append(cmd.Env, envList...)
	// override envs from the origEnvs
	cmd.Env = append(cmd.Env, origEnvs...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("failed to run the ovms server launch : %v", err)
		log.Printf("output: %v", string(output))
		// based on the startup policy when there is error on launching ovms server,
		// it will deal it differently:
		switch startupPolicy {
		case Exit.String():
			os.Exit(1)
		case RemoveRestart.String():
			rmvContainerName := ovmsClientConf.OvmsServer.ServerContainerName + os.Getenv(CID_COUNT_ENV)
			rmvCmd := exec.Command("docker", []string{"rm", "-f", rmvContainerName}...)
			if rmvErr := rmvCmd.Run(); rmvErr != nil {
				log.Printf("failed to remove the existing container with container name %s: %v", rmvContainerName, rmvErr)
			}
			time.Sleep(time.Second)
			startOvmsServer(ovmsClientConf)
		default:
			fallthrough
		case Ignore.String():
			log.Println("startup error is ignored due to ignore startup policy")
		}
	}

	ovmsSrvWaitTime := defaultOvmsServerStartWaitTime
	if len(ovmsClientConf.OvmsServer.InitWaitTime) > 0 {
		ovmsSrvWaitTime, err = time.ParseDuration(ovmsClientConf.OvmsServer.InitWaitTime)
		if err != nil {
			log.Printf("Error parsing ovmsClientConf.OvmsServer.InitWaitTime %s, using default value %v : %s",
				ovmsClientConf.OvmsServer.InitWaitTime, defaultOvmsServerStartWaitTime, err)
			ovmsSrvWaitTime = defaultOvmsServerStartWaitTime
		}
	}

	log.Println("Let server settle a bit...")
	time.Sleep(ovmsSrvWaitTime)
	log.Println("OVMS server started")
}

func launchPipelineScript(ovmsClientConf OvmsClientConfig) error {
	scriptFilePath := filepath.Join(scriptDir, ovmsClientConf.OvmsClient.PipelineScript)
	inputArgs := parseInputArguments(ovmsClientConf)
	// if running in stream density mode, then the executable is the stream_density shell script itself with input
	streamDensityMode := os.Getenv("STREAM_DENSITY_MODE")
	pipelineStreamDensityRun := strings.TrimSpace(ovmsClientConf.OvmsClient.PipelineStreamDensityRun)
	if streamDensityMode == "1" {
		log.Println("in stream density mode!")
		if len(pipelineStreamDensityRun) == 0 {
			// when pipelineStreamDensityRun is empty string, then default to the original pipelineScript
			pipelineStreamDensityRun = filepath.Join(scriptDir, ovmsClientConf.OvmsClient.PipelineScript)
			scriptFilePath = streamDensityScript
			inputArgs = []string{filepath.Join(pipelineStreamDensityRun + commandLineArgsDelimiter +
				ovmsClientConf.OvmsClient.PipelineInputArgs)}
		}
	}

	executable, err := exec.LookPath(scriptFilePath)
	if err != nil {
		return fmt.Errorf("failed to get pipeline executable path: %v", err)
	}

	log.Println("running executable:", executable)
	cmd := exec.Command(executable, inputArgs...)
	cmd.Env = os.Environ()
	if streamDensityMode == "1" {
		cmd.Env = append(cmd.Env, "PipelineStreamDensityRun="+pipelineStreamDensityRun)
	}

	// in order to do the environment override from the current existing cmd.Env,
	// we have to save this and then apply the overrides with the existing keys
	origEnvs := make([]string, len(cmd.Env))
	copy(origEnvs, cmd.Env)
	// apply all envs from env files if any
	envList := ovmsClientConf.OvmsClient.readClientEnvs(envFileDir)
	cmd.Env = append(cmd.Env, envList...)
	// override envs from the origEnvs
	cmd.Env = append(cmd.Env, origEnvs...)

	envs := cmd.Env
	for _, env := range envs {
		log.Println("environment variable: ", env)
	}
	if len(envs) == 0 {
		log.Println("empty environment variable")
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get the output from executable: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get the error pipe from executable: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start the pipeline executable: %v", err)
	}

	stdoutBytes, _ := io.ReadAll(stdout)
	log.Println("stdoutBytes: ", string(stdoutBytes))
	stdErrBytes, _ := io.ReadAll(stderr)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("found error while executing pipeline scripts- stdErrMsg: %s, Err: %v", string(stdErrBytes), err)
	}

	log.Println(string(stdoutBytes))
	return nil
}

func parseInputArguments(ovmsClientConf OvmsClientConfig) []string {
	inputArgs := []string{}
	trimmedArgs := strings.TrimSpace(ovmsClientConf.OvmsClient.PipelineInputArgs)
	if len(trimmedArgs) > 0 {
		// arguments in command is space delimited
		return strings.Split(trimmedArgs, commandLineArgsDelimiter)
	}
	return inputArgs
}

func (flags *Flags) readPipelineConfig() ([]byte, error) {
	var contents []byte
	var err error

	pipelineConfig := filepath.Join(resourceDir, pipelineConfigFileName)
	pipelineProfile := strings.TrimSpace(os.Getenv(pipelineProfileEnv))
	// if pipelineProfile is empty, then will default to the current folder
	if len(pipelineProfile) == 0 {
		log.Printf("Loading configuration yaml file from %s folder...", flags.configDir)
		pipelineConfig = filepath.Join(flags.configDir, pipelineConfig)
	} else {
		log.Println("pipelineProfile: ", pipelineProfile)
		pipelineConfig = filepath.Join(flags.configDir, resourceDir, pipelineProfile, pipelineConfigFileName)
	}

	contents, err = os.ReadFile(pipelineConfig)
	if err != nil {
		err = fmt.Errorf("readPipelineConfig error with pipelineConfig: %v, environment variable key for pipelineProfile: %v, error: %v",
			pipelineConfig, pipelineProfileEnv, err)
	}

	return contents, err
}
