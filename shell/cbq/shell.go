//  Copyright 2015-Present Couchbase, Inc.
//
//  Use of this software is governed by the Business Source License included
//  in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
//  in that file, in accordance with the Business Source License, use of this
//  software will be governed by the Apache License, Version 2.0, included in
//  the file licenses/APL2.txt.

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/couchbase/godbc/n1ql"
	"github.com/couchbase/query/errors"
	"github.com/couchbase/query/shell/cbq/command"
)

/*
   Command line options provided.
*/

/*
   Option        : -engine or -e
   Args          :  <url to the query service or to the cluster>
   Default value : http://localhost:8091/
   Point to the cluser/query endpoint to connect to.
*/
var serverFlag string

func init() {
	const (
		defaultServer = "http://localhost:8091/"
		usage         = command.USERVERFLAG
	)
	flag.StringVar(&serverFlag, "engine", defaultServer, usage)
	flag.StringVar(&serverFlag, "e", defaultServer, command.NewShorthandMsg("-engine"))
}

/*
   Option        : -no-engine or -ne
   Default value : false
   Enable/Disable startup connection to a query service/cluster endpoint.
*/
var noQueryService bool

func init() {
	const (
		defaultval = false
		usage      = command.UNOENGINE
	)
	flag.BoolVar(&noQueryService, "no-engine", defaultval, usage)
	flag.BoolVar(&noQueryService, "ne", defaultval, command.NewShorthandMsg("-no-engine"))
}

/*
   Option        : -quiet
   Default value : false
   Enable/Disable startup connection message for the shell. Also disable echoing queries
   when using \SOURCE or -f.
*/
var quietFlag bool

func init() {
	const (
		defaultval = false
		usage      = command.UQUIET
	)
	flag.BoolVar(&quietFlag, "quiet", defaultval, usage)
	flag.BoolVar(&quietFlag, "q", defaultval, command.NewShorthandMsg("-quiet"))
}

/*
   Option        : -timeout or -t
   Args          : <timeout value>
   Default value : "0ms"
   Query timeout parameter.
*/

var timeoutFlag string

func init() {
	const (
		defaultval = ""
		usage      = command.UTIMEOUT
	)
	flag.StringVar(&timeoutFlag, "timeout", defaultval, usage)
	flag.StringVar(&timeoutFlag, "t", defaultval, command.NewShorthandMsg("-timeout"))
}

/*
   Option        : -user or -u
   Args          : Login username
   Login credentials for users. The shell will prompt for the password.
*/

var userFlag string

func init() {
	const (
		defaultval = ""
		usage      = command.UUSER
	)
	flag.StringVar(&userFlag, "user", defaultval, usage)
	flag.StringVar(&userFlag, "u", defaultval, command.NewShorthandMsg("-user"))

}

/*
   Option        : -password, passphrase or -p
   Args          : password for username or passphrase for keyfile (Private Key Encryption)
   Password for user given by -u. If -u is present and we provide -p, then
   do not prompt for the password. Error out if username is not provided.
*/

var pwdFlag string

func init() {
	const (
		defaultval = ""
		usage      = command.UPWD
	)
	flag.StringVar(&pwdFlag, "password", defaultval, usage)
	flag.StringVar(&pwdFlag, "p", defaultval, command.NewShorthandMsg("-password"))

}

/*
   Option        : -passphrase or -pp
   Args          : passphrase for keyfile (Private Key Encryption)
   Need to provide certfile and keyfile with passphrase
*/

var passpFlag string

func init() {
	const (
		defaultval = ""
		usage      = command.UPP
	)
	flag.StringVar(&passpFlag, "passphrase", defaultval, usage)
	flag.StringVar(&passpFlag, "pp", defaultval, command.NewShorthandMsg("-passphrase"))

}

/*
   Option        : -credentials or -c
   Args          : A list of credentials, in the form of user/password objects.
   Login credentials for users as well as SASL Buckets.
*/

var credsFlag string

func init() {
	const (
		defaultval = ""
		usage      = command.UCREDS
	)
	flag.StringVar(&credsFlag, "credentials", defaultval, usage)
	flag.StringVar(&credsFlag, "c", defaultval, command.NewShorthandMsg("-credentials"))

}

/*
   Option        : -version or -v
   Shell Version
*/

var versionFlag bool

func init() {
	const (
		usage = command.UVERSION
	)
	flag.BoolVar(&versionFlag, "version", false, usage)
	flag.BoolVar(&versionFlag, "v", false, command.NewShorthandMsg("-version"))

}

/*
   Option        : -script or -s
   Args          : <query>
   Single command mode
*/

type scripts []string

var scriptFlag scripts

func init() {
	const (
		usage = command.USCRIPT
	)
	flag.Var(&scriptFlag, "script", usage)
	flag.Var(&scriptFlag, "s", command.NewShorthandMsg("-script"))

}

func (s *scripts) String() string {
	return fmt.Sprintf("%s", *s)
}

func (s *scripts) Set(val string) error {
	*s = append(*s, val)
	return nil
}

/*
   Option        : -pretty
   Default value : true
   Pretty print output
*/

var prettyFlag = flag.Bool("pretty", true, command.UPRETTY)

/*
   Option        : -terse
   Default value : false
   Terse output
*/

var terseFlag = flag.Bool("terse", false, command.UTERSE)

/*
   Option        : -exit-on-error
   Default value : false
   Exit shell after first error encountered.
*/

var errorExitFlag = flag.Bool("exit-on-error", false, command.UEXIT)

/*
   Option        : -file or -f
   Args          : <filename>
   Input file to run queries from. Exit after the queries are run.
*/

var inputFlag string

func init() {
	const (
		defaultval = ""
		usage      = command.UINPUT
	)
	flag.StringVar(&inputFlag, "file", defaultval, usage)
	flag.StringVar(&inputFlag, "f", defaultval, command.NewShorthandMsg("-file"))

}

/*
   Option        : -ouput or -o
   Args          : <filename>
   Output file to send results of queries to.
*/

var outputFlag string

func init() {
	const (
		defaultval = ""
		usage      = command.UOUTPUT
	)
	flag.StringVar(&outputFlag, "output", defaultval, usage)
	flag.StringVar(&outputFlag, "o", defaultval, command.NewShorthandMsg("-output"))

}

/*
   Option        : -no-ssl-verify
   Default Value : false
   Skip verification of Certificates.
*/

var noSSLVerify bool

func init() {
	const (
		defaultval = false
		usage      = command.USSLVERIFY
	)
	flag.BoolVar(&noSSLVerify, "no-ssl-verify", defaultval, usage)
	flag.BoolVar(&noSSLVerify, "skip-verify", defaultval, "Synonym for no-ssl-verify.")

}

/*
   Option        : -cacert
   Args : <path to root ca certificate>
   Pass path to root ca certificate to verify identity of server.
*/

var rootFile string

func init() {
	const (
		defaultval = ""
		usage      = command.UCACERT
	)
	flag.StringVar(&rootFile, "cacert", defaultval, usage)
}

/*
   Option        : -cert
   Args : <path to chain certificate>
   Pass path to chain certificate.
*/

var certFile string

func init() {
	const (
		defaultval = ""
		usage      = command.UCERTFILE
	)
	flag.StringVar(&certFile, "cert", defaultval, usage)
}

/*
   Option        : -key
   Args : <path to client key>
   Pass path to client key file.
*/

var keyFile string

func init() {
	const (
		defaultval = ""
		usage      = command.UKEYFILE
	)
	flag.StringVar(&keyFile, "key", defaultval, usage)
}

/* Define credentials as user/pass and convert into
   JSON object credentials
*/

/*
   Option        : -batch or -b
   Args          : on/off
   Batch mode for sending queries to Asterix.
*/

var batchFlag string

func init() {
	const (
		defaultval = "off"
		usage      = command.UBATCH
	)
	flag.StringVar(&batchFlag, "batch", defaultval, usage)
	flag.StringVar(&batchFlag, "b", defaultval, command.NewShorthandMsg("-batch"))
}

/*
   Option        : -analytics or -a
   Args          : true or false
   Used to send queries to Asterix/auto discovering analytics nodes in a cluster.
*/

var analyticsFlag bool

func init() {
	const (
		defaultval = false
		usage      = command.UANALYTICS
	)
	flag.BoolVar(&analyticsFlag, "analytics", defaultval, usage)
	flag.BoolVar(&analyticsFlag, "a", defaultval, command.NewShorthandMsg("-analytics"))
}

/*
   Option        : -networkconfig or -ncfg
   Args          : String (default or external supported as of now)
   Alternate address support for connection to server. Auto indicates that we auto-discover if the input IP is
   an internal or external address.
*/

var networkconfigFlag string

func init() {
	const (
		defaultval = "auto"
		usage      = command.UNETWORK
	)
	flag.StringVar(&networkconfigFlag, "networkconfig", defaultval, usage)
	flag.StringVar(&networkconfigFlag, "ncfg", defaultval, command.NewShorthandMsg("-networkconfig"))
}

/*
   Option        : -vi
   single-line vi style input mode
*/

var viModeSingleLineFlag bool

func init() {
	const (
		usage = command.UVIMODESL
	)
	flag.BoolVar(&viModeSingleLineFlag, "vi", false, usage)
}

/*
   Option        : -vim
   multi-line vi style input mode
*/

var viModeMultiLineFlag bool

func init() {
	const (
		usage = command.UVIMODEML
	)
	flag.BoolVar(&viModeMultiLineFlag, "vim", false, usage)
}

/*
   Option        : -history or -hist
   Args          : Path to load query history from.

   Load history (query history) from input path and use that to save query statements as well.
*/

var histFlag string

func init() {
	const (
		defaultval = ".cbq_history"
		usage      = command.HISTORYMSG
	)
	flag.StringVar(&histFlag, "history", defaultval, usage)
	flag.StringVar(&histFlag, "hist", defaultval, command.NewShorthandMsg("history"))
}

var (
	SERVICE_URL  string
	DISCONNECT   bool
	EXIT         bool
	stringBuffer bytes.Buffer
)

func main() {

	flag.Parse()

	// Initialize Global buffer to store queries for batch mode.
	stringBuffer.Write([]byte(""))
	command.BATCH = batchFlag
	err_code, err_str := command.PushValue_Helper(true, command.PreDefSV, "batch", batchFlag)
	if err_code != 0 {
		s_err := command.HandleError(err_code, err_str)
		command.PrintError(s_err)
	}

	if *prettyFlag {
		n1ql.SetQueryParams("pretty", "true")
	} else {
		n1ql.SetQueryParams("pretty", "false")
	}

	command.TERSE = *terseFlag
	if *terseFlag {
		n1ql.SetQueryParams("signature", "false")
	}

	if outputFlag != "" {
		// Redirect all output to the given file.
		// This is handled in the HandleInteractiveMode() method
		// in interactive.go.
		command.FILE_RW_MODE = true
		command.FILE_OUTPUT = outputFlag
	}

	// Set command.W = os.Stdout
	command.SetWriter(os.Stdout)

	/* Handle options and what they should do */

	/* -version : Display the version of the shell and then exit.
	 */
	if versionFlag == true {
		dummy := []string{}
		cmd := command.Version{}
		cmd.ExecCommand(dummy)
		os.Exit(0)
	}

	/* Check for input url argument
	 */

	args := flag.Args()
	if len(args) > 1 {
		s_err := command.HandleError(errors.E_SHELL_CMD_LINE_ARGS, "")
		command.PrintError(s_err)
		os.Exit(1)
	} else {
		if len(args) == 1 {
			serverFlag = args[0]
		}
	}

	n1ql.SetNetworkType(networkconfigFlag)
	n1ql.SetIsAnalytics(analyticsFlag)

	// call command.ParseURL()
	var errCode errors.ErrorCode
	var errStr string
	var pURL *command.UrlRes

	pURL, errCode, errStr = command.ParseURL(serverFlag)
	if errCode != 0 {
		s_err := command.HandleError(errCode, errStr)
		command.PrintError(s_err)
		os.Exit(1)
	}

	serverFlag = pURL.ServerFlag

	//-engine
	if strings.HasSuffix(serverFlag, "/") == false {
		serverFlag = serverFlag + "/"
	}

	/* -user : Accept Admin credentials. Prompt for password and set
	   the n1ql_creds. Append to creds so that user can also define
	   bucket credentials using -credentials if they need to.
	*/
	var creds command.Credentials
	var err error
	var password []byte

	if userFlag != "" {
		//Check if there is a -password option.
		if pwdFlag != "" {
			password = []byte(pwdFlag)
			err = nil
		} else {
			// If no -p option then prompt for the password
			password, err = promptPassword(command.PWDMSG)
		}

		if err == nil {
			if string(password) == "" {
				s_err := command.HandleError(errors.E_SHELL_INVALID_PASSWORD, "")
				command.PrintError(s_err)
				os.Exit(1)
			} else {
				// Error out in cases where password contains escape sequences
				// or ctrl chars.
				for _, c := range bytes.Runes(password) {
					if !unicode.IsPrint(c) {
						s_err := command.HandleError(errors.E_SHELL_INVALID_PASSWORD, "")
						command.PrintError(s_err)
						os.Exit(1)
					}
				}

				creds = append(creds, command.Credential{"user": userFlag, "pass": string(password)})
				// The driver needs the username/password to query the cluster endpoint,
				// which may require authorization.
				n1ql.SetUsernamePassword(userFlag, string(password))
			}
		} else {
			s_err := command.HandleError(errors.E_SHELL_INVALID_PASSWORD, err.Error())
			command.PrintError(s_err)
			os.Exit(1)
		}
	} else {
		// If the -u option isnt specified and the -p option is specified
		// It could be using the passphrase instead of the username.
		// Make sure the keyfile is also empty before throwing the error.
		if pwdFlag != "" {
			s_err := command.HandleError(errors.E_SHELL_INVALID_USERNAME, "")
			command.PrintError(s_err)
			os.Exit(1)
		}
	}

	/* -credentials : Accept credentials to pass to the n1ql endpoint.
	   Ensure that the user inputs credentials in the form a:b.
	   It is important to apend these credentials to those given by
	   -user.
	*/
	if userFlag == "" && credsFlag == "" {
		// No credentials exist. This can still be used to connect to
		// un-authenticated servers.
		// Dont output the statement if we are running in single command
		// mode.
		if len(scriptFlag) == 0 && rootFile == "" && certFile == "" && keyFile == "" {
			_, werr := io.WriteString(command.W, command.STARTUPCREDS)

			if werr != nil {
				s_err := command.HandleError(errors.E_SHELL_WRITER_OUTPUT, werr.Error())
				command.PrintError(s_err)
			}
		}

	} else if credsFlag != "" {

		creds_ret, err_code, err_string := command.ToCreds(credsFlag)
		if err_code != 0 {
			s_err := command.HandleError(err_code, err_string)
			command.PrintError(s_err)
		}
		for _, v := range creds_ret {
			creds = append(creds, v)
		}

	}

	// Append credentials part of the input server Flag to creds if they exist
	if pURL.Username != "" && pURL.Password != "" {
		creds = append(creds, command.Credential{"user": pURL.Username, "pass": pURL.Password})
	}

	/* Add the credentials set by -user and -credentials to the
	   n1ql creds parameter.
	*/
	if creds != nil {
		ac, err := json.Marshal(creds)
		if err != nil {
			//Error while Marshalling
			s_err := command.HandleError(errors.E_SHELL_JSON_MARSHAL, err.Error())
			command.PrintError(s_err)
			os.Exit(1)
		}
		n1ql.SetQueryParams("creds", string(ac))
		n1ql.SetUsernamePassword(creds[0]["user"], creds[0]["pass"])
	}

	n1ql.SetSkipVerify(noSSLVerify)
	command.SKIPVERIFY = noSSLVerify

	if certFile != "" {
		n1ql.SetCertFile(certFile)
	}

	if keyFile != "" {
		n1ql.SetKeyFile(keyFile)
	}

	if rootFile != "" {
		n1ql.SetCaFile(rootFile)
	}

	if certFile != "" && keyFile != "" {
		n1ql.SetPrivateKeyPassphrase([]byte(passpFlag))
	}

	if strings.HasPrefix(strings.ToLower(serverFlag), "https://") && rootFile == "" && certFile == "" && keyFile == "" {
		if noSSLVerify == false {
			command.PrintStr(command.W, command.SSLVERIFY_FALSE)
		} else {
			command.PrintStr(command.W, command.SSLVERIFY_TRUE)
		}
	}

	if noQueryService == false {
		// Check if connection is possible to the input serverFlag
		// else failed to connect to. This establishes the dbn1ql handle for future queries
		pingerr := command.Ping(serverFlag)
		SERVICE_URL = serverFlag
		command.SERVICE_URL = serverFlag
		if pingerr != nil && strings.Contains(pingerr.Error(), "parsePrivateKey") {
			// Prompt for passphrase and retry
			newPassp, err := promptPassword(command.PASSPMSG)
			if err == nil {
				n1ql.SetPrivateKeyPassphrase(newPassp)
				pingerr = command.Ping(serverFlag)
			}
		}

		if pingerr != nil {
			s_err := command.HandleError(errors.E_SHELL_CONNECTION_REFUSED, pingerr.Error())
			command.PrintError(s_err)
			serverFlag = ""
			command.SERVICE_URL = ""
			SERVICE_URL = ""
			noQueryService = true
		}

		/* -quiet : Display Message only if flag not specified
		 */
		if !quietFlag && pingerr == nil {
			s := command.NewMessage(command.STARTUP, fmt.Sprintf("%v", serverFlag)) + command.EXITMSG
			_, werr := io.WriteString(command.W, s)
			if werr != nil {
				s_err := command.HandleError(errors.E_SHELL_WRITER_OUTPUT, werr.Error())
				command.PrintError(s_err)
			}
		}
	}

	// Verify histfile input path is valid.
	if histFlag != "" {
		command.HISTFILE = histFlag
	}

	//Set QUIET to enable/disable histfile path message
	//If quiet is true
	command.QUIET = quietFlag
	if quietFlag {
		// SET the quiet mode here
		//SET batch mode here
		err_code, err_str := command.PushValue_Helper(true, command.PreDefSV, "quiet", strconv.FormatBool(quietFlag))
		if err_code != 0 {
			s_err := command.HandleError(err_code, err_str)
			command.PrintError(s_err)
		}
	}

	if timeoutFlag != "0ms" && timeoutFlag != "" {
		n1ql.SetQueryParams("timeout", timeoutFlag)
	}

	n1ql.SetCBUserAgentHeader("CBQ/" + command.SHELL_VERSION)

	// Handle the inputFlag and ScriptFlag options in HandleInteractiveMode.
	// This is so as to add these to the history.

	HandleInteractiveMode(filepath.Base(os.Args[0]))
}
