package main

import (
	"flag"
	"fmt"
	"strings"
)

const completionUsage = `Usage:
  wally completion bash|zsh|fish|powershell

Print a shell completion script to stdout. The scripts register completion
for wally, nanobot and nanobot-go. PowerShell also accepts the alias "pwsh".
`

func (a *app) completion(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(a.stderr, completionUsage)
		if len(args) == 0 {
			return errUsage
		}
		return flag.ErrHelp
	}
	if len(args) != 1 {
		fmt.Fprintf(a.stderr, "wally completion: want one shell, got %q\n\n%s", args, completionUsage)
		return errUsage
	}
	shell := strings.ToLower(args[0])
	if shell == "pwsh" {
		shell = "powershell"
	}
	script, ok := completionScripts[shell]
	if !ok {
		fmt.Fprintf(a.stderr, "wally completion: unknown shell %q\n\n%s", args[0], completionUsage)
		return errUsage
	}
	fmt.Fprint(a.stdout, script)
	return nil
}

var completionScripts = map[string]string{
	"bash":       bashCompletion,
	"zsh":        zshCompletion,
	"fish":       fishCompletion,
	"powershell": powershellCompletion,
}

const bashCompletion = `# bash completion for wally, nanobot and nanobot-go
_nanobot()
{
    local cur prev path word i
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"
    path=""
    for ((i=1; i<COMP_CWORD; i++)); do
        word="${COMP_WORDS[i]}"
        [[ "$word" == -- ]] && break
        [[ "$word" == -* ]] && continue
        case "$path:$word" in
            :*) path="$word" ;;
            cron:list|cron:remove|cron:rm|cron:run|pairing:list|pairing:ls|pairing:approve|pairing:deny|pairing:revoke|channels:login|channels\ login:weixin|gateway:status|gateway:stop|gateway:restart|gateway:logs|gateway:service|gateway\ service:install|gateway\ service:uninstall|gateway\ service:start|gateway\ service:stop|gateway\ service:status) path="$path $word" ;;
        esac
    done
    case "$prev" in
        --config|--workspace) compopt -o filenames; COMPREPLY=( $(compgen -f -- "$cur") ); return ;;
    esac
    local choices
    case "$path" in
        "") choices="onboard agent gateway serve cron pairing channels dream webui status version help completion" ;;
        completion) choices="bash zsh fish powershell pwsh -h --help" ;;
        cron) choices="list remove rm run -h --help" ;;
        "cron list") choices="--all --config --workspace -h --help" ;;
        "cron remove"|"cron rm") choices="--config --workspace -h --help" ;;
        "cron run") choices="--force --config --workspace --verbose -h --help" ;;
        pairing) choices="list ls approve deny revoke -h --help" ;;
        "pairing "*) choices="--config -h --help" ;;
        channels) choices="login -h --help" ;;
        "channels login") choices="weixin weixin.<name> --force --config -h --help" ;;
        "channels login weixin") choices="--force --config -h --help" ;;
        gateway) choices="status stop restart logs service install-service uninstall-service --background --config --workspace --host --port --verbose -h --help" ;;
        "gateway status") choices="--config --workspace -h --help" ;;
        "gateway stop") choices="--config --workspace --timeout -h --help" ;;
        "gateway restart") choices="--config --workspace --host --port --verbose --timeout -h --help" ;;
        "gateway logs") choices="--config --workspace --tail -h --help" ;;
        "gateway service") choices="install uninstall start restart stop status -h --help" ;;
		"gateway service install"|"gateway install-service") choices="--config --workspace --host --port --verbose --enable --no-enable --start --no-start --dry-run --name --manager -h --help" ;;
		"gateway service uninstall"|"gateway uninstall-service"|"gateway service start"|"gateway service restart"|"gateway service stop"|"gateway service status") choices="--config --workspace --name --manager --dry-run -h --help" ;;
        agent) choices="-m --message -s --session --config --workspace --verbose -h --help" ;;
        onboard) choices="--config --force -h --help" ;;
        serve) choices="--config --workspace --host --port --timeout --verbose -h --help" ;;
        dream) choices="--config --workspace --verbose -h --help" ;;
        webui) choices="--config --workspace --host --port -p --gateway-port --dev --no-open --yes --background -h --help" ;;
        status) choices="--config --mcp -h --help" ;;
        *) choices="-h --help" ;;
    esac
    COMPREPLY=( $(compgen -W "$choices" -- "$cur") )
}
complete -F _nanobot wally nanobot nanobot-go
`

const zshCompletion = `#compdef wally nanobot nanobot-go

_nanobot() {
  local -a words_before choices
  local path="" word
  words_before=("${words[@]:2:$CURRENT-2}")
  for word in $words_before; do
    [[ "$word" == -- ]] && break
    [[ "$word" == -* ]] && continue
    case "$path:$word" in
      :*) path="$word" ;;
      cron:(list|remove|rm|run)|pairing:(list|ls|approve|deny|revoke)|channels:login|'channels login':weixin|gateway:(status|stop|restart|logs|service|install-service|uninstall-service)|'gateway service':(install|uninstall|start|restart|stop|status)) path="$path $word" ;;
    esac
  done
  case "${words[CURRENT-1]}" in
    --config|--workspace) _files; return ;;
  esac
  case "$path" in
    '') choices=(onboard agent gateway serve cron pairing channels dream webui status version help completion) ;;
    completion) choices=(bash zsh fish powershell pwsh -h --help) ;;
    cron) choices=(list remove rm run -h --help) ;;
    'cron list') choices=(--all --config --workspace -h --help) ;;
    'cron remove'|'cron rm') choices=(--config --workspace -h --help) ;;
    'cron run') choices=(--force --config --workspace --verbose -h --help) ;;
    pairing) choices=(list ls approve deny revoke -h --help) ;;
    pairing\ *) choices=(--config -h --help) ;;
    channels) choices=(login -h --help) ;;
    'channels login'|'channels login weixin') choices=(weixin 'weixin.<name>' --force --config -h --help) ;;
    gateway) choices=(status stop restart logs service install-service uninstall-service --background --config --workspace --host --port --verbose -h --help) ;;
    'gateway status') choices=(--config --workspace -h --help) ;;
    'gateway stop') choices=(--config --workspace --timeout -h --help) ;;
    'gateway restart') choices=(--config --workspace --host --port --verbose --timeout -h --help) ;;
    'gateway logs') choices=(--config --workspace --tail -h --help) ;;
    'gateway service') choices=(install uninstall start restart stop status -h --help) ;;
	'gateway service install'|'gateway install-service') choices=(--config --workspace --host --port --verbose --enable --no-enable --start --no-start --dry-run --name --manager -h --help) ;;
	'gateway service '*|'gateway uninstall-service') choices=(--config --workspace --name --manager --dry-run -h --help) ;;
    agent) choices=(-m --message -s --session --config --workspace --verbose -h --help) ;;
    onboard) choices=(--config --force -h --help) ;;
    serve) choices=(--config --workspace --host --port --timeout --verbose -h --help) ;;
    dream) choices=(--config --workspace --verbose -h --help) ;;
    webui) choices=(--config --workspace --host --port -p --gateway-port --dev --no-open --yes --background -h --help) ;;
    status) choices=(--config --mcp -h --help) ;;
    *) choices=(-h --help) ;;
  esac
  compadd -- $choices
}

_nanobot "$@"
`

const fishCompletion = `# fish completion for wally, nanobot and nanobot-go
function __nanobot_seen
    contains -- $argv[1] (commandline -opc)[2..-1]
end
function __nanobot_top
    not __nanobot_seen onboard; and not __nanobot_seen agent; and not __nanobot_seen gateway; and not __nanobot_seen serve; and not __nanobot_seen cron; and not __nanobot_seen pairing; and not __nanobot_seen channels; and not __nanobot_seen dream; and not __nanobot_seen webui; and not __nanobot_seen status; and not __nanobot_seen version; and not __nanobot_seen help; and not __nanobot_seen completion
end
for cmd in wally nanobot nanobot-go
    complete -c $cmd -f -n __nanobot_top -a 'onboard agent gateway serve cron pairing channels dream webui status version help completion'
    complete -c $cmd -f -n '__nanobot_seen completion' -a 'bash zsh fish powershell pwsh'
    complete -c $cmd -f -n '__nanobot_seen cron; and not __nanobot_seen list; and not __nanobot_seen remove; and not __nanobot_seen rm; and not __nanobot_seen run' -a 'list remove rm run'
    complete -c $cmd -f -n '__nanobot_seen pairing; and not __nanobot_seen list; and not __nanobot_seen ls; and not __nanobot_seen approve; and not __nanobot_seen deny; and not __nanobot_seen revoke' -a 'list ls approve deny revoke'
    complete -c $cmd -f -n '__nanobot_seen channels; and not __nanobot_seen login' -a login
    complete -c $cmd -f -n '__nanobot_seen channels; and __nanobot_seen login' -a weixin
    complete -c $cmd -f -n '__nanobot_seen gateway; and not __nanobot_seen status; and not __nanobot_seen stop; and not __nanobot_seen restart; and not __nanobot_seen logs; and not __nanobot_seen service' -a 'status stop restart logs service install-service uninstall-service'
    complete -c $cmd -f -n '__nanobot_seen gateway; and __nanobot_seen service' -a 'install uninstall start stop status'
    complete -c $cmd -l config -r -F
    complete -c $cmd -l workspace -r -F
    complete -c $cmd -s h -l help
    complete -c $cmd -n '__nanobot_seen agent' -s m -l message -r
    complete -c $cmd -n '__nanobot_seen agent' -s s -l session -r
    complete -c $cmd -n '__nanobot_seen agent; or __nanobot_seen gateway; or __nanobot_seen serve; or __nanobot_seen dream' -l verbose
    complete -c $cmd -n '__nanobot_seen gateway; or __nanobot_seen serve; or __nanobot_seen webui' -l host -r
    complete -c $cmd -n '__nanobot_seen gateway; or __nanobot_seen serve; or __nanobot_seen webui' -l port -r
    complete -c $cmd -n '__nanobot_seen gateway; or __nanobot_seen webui' -l background
    complete -c $cmd -n '__nanobot_seen cron; or __nanobot_seen onboard; or __nanobot_seen channels' -l force
    complete -c $cmd -n '__nanobot_seen status' -l mcp
    complete -c $cmd -n '__nanobot_seen webui' -l dev
    complete -c $cmd -n '__nanobot_seen webui' -l no-open
    complete -c $cmd -n '__nanobot_seen webui' -l yes
	complete -c $cmd -n '__nanobot_seen service; or __nanobot_seen install-service; or __nanobot_seen uninstall-service' -l name -r
	complete -c $cmd -n '__nanobot_seen service; or __nanobot_seen install-service; or __nanobot_seen uninstall-service' -l manager -r -a 'auto launchd systemd'
	complete -c $cmd -n '__nanobot_seen service; or __nanobot_seen install-service; or __nanobot_seen uninstall-service' -l dry-run
end
`

const powershellCompletion = `# PowerShell completion for wally, nanobot and nanobot-go
$nanobotCompleter = {
    param($wordToComplete, $commandAst, $cursorPosition)
    $tokens = @($commandAst.CommandElements | ForEach-Object { $_.ToString() })
    $path = if ($tokens.Count -gt 1) { $tokens[1] } else { '' }
    if ($tokens.Count -gt 2) { $path += ' ' + $tokens[2] }
    if ($tokens.Count -gt 3 -and $tokens[1] -eq 'gateway' -and $tokens[2] -eq 'service') { $path += ' ' + $tokens[3] }
    $choices = switch -Wildcard ($path) {
        '' { 'onboard','agent','gateway','serve','cron','pairing','channels','dream','webui','status','version','help','completion' }
        'completion*' { 'bash','zsh','fish','powershell','pwsh','-h','--help' }
		'cron' { 'list','remove','rm','run','-h','--help' }
        'cron list*' { '--all','--config','--workspace','-h','--help' }
        'cron remove*' { '--config','--workspace','-h','--help' }
        'cron rm*' { '--config','--workspace','-h','--help' }
        'cron run*' { '--force','--config','--workspace','--verbose','-h','--help' }
		'pairing' { 'list','ls','approve','deny','revoke','-h','--help' }
        'pairing *' { '--config','-h','--help' }
		'channels' { 'login','-h','--help' }
        'channels login*' { 'weixin','--force','--config','-h','--help' }
		'gateway' { 'status','stop','restart','logs','service','install-service','uninstall-service','--background','--config','--workspace','--host','--port','--verbose','-h','--help' }
		'gateway service' { 'install','uninstall','start','restart','stop','status','-h','--help' }
		'gateway service install*' { '--config','--workspace','--host','--port','--verbose','--enable','--no-enable','--start','--no-start','--dry-run','--name','--manager','-h','--help' }
		'gateway service *' { '--config','--workspace','--name','--manager','--dry-run','-h','--help' }
		'gateway install-service*' { '--config','--workspace','--host','--port','--verbose','--enable','--no-enable','--start','--no-start','--dry-run','--name','--manager','-h','--help' }
		'gateway uninstall-service*' { '--config','--workspace','--name','--manager','--dry-run','-h','--help' }
        'gateway status*' { '--config','--workspace','-h','--help' }
        'gateway stop*' { '--config','--workspace','--timeout','-h','--help' }
        'gateway restart*' { '--config','--workspace','--host','--port','--verbose','--timeout','-h','--help' }
        'gateway logs*' { '--config','--workspace','--tail','-h','--help' }
        'agent*' { '-m','--message','-s','--session','--config','--workspace','--verbose','-h','--help' }
        'onboard*' { '--config','--force','-h','--help' }
        'serve*' { '--config','--workspace','--host','--port','--timeout','--verbose','-h','--help' }
        'dream*' { '--config','--workspace','--verbose','-h','--help' }
        'webui*' { '--config','--workspace','--host','--port','-p','--gateway-port','--dev','--no-open','--yes','--background','-h','--help' }
        'status*' { '--config','--mcp','-h','--help' }
        default { '-h','--help' }
    }
    $choices | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object {
        [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
    }
}
Register-ArgumentCompleter -Native -CommandName wally,nanobot,nanobot-go -ScriptBlock $nanobotCompleter
`
