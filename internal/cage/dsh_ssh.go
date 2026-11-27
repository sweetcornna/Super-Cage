package cage

import _ "embed"

//go:embed dsh-ssh.py
var dshSSHHelper string

func dshSSHScript() string {
	return "sudo install -d -m 0755 /usr/local/lib/cage\n" +
		"sudo tee /usr/local/lib/cage/dsh-ssh.py >/dev/null <<'CAGE_DSH_SSH'\n" + dshSSHHelper + "\nCAGE_DSH_SSH\n" +
		`sudo chmod 0644 /usr/local/lib/cage/dsh-ssh.py
sudo tee /etc/profile.d/cage-dsh.sh >/dev/null <<'CAGE_DSH_PROFILE'
# SSH users keep their normal DeepSeek launcher and its installed profile.
if [ -n "${SSH_CONNECTION:-}${CAGE_DSH_SSH_DEVICE:-}" ]; then
  _cage_dsh_native() {
    unset -f dsh
    command -v dsh
  }
  dsh() {
    local native
    native=$(_cage_dsh_native) || { command dsh "$@"; return; }
    /usr/bin/python3 /usr/local/lib/cage/dsh-ssh.py "$native" "$@"
  }

  _cage_dsh_first_prompt() {
    local status=$? native
    if [ -z "${_cage_dsh_prompt_seen:-}" ]; then
      _cage_dsh_prompt_seen=1
      if [ -t 0 ] && [ -t 1 ] && [ -z "${TMUX:-}" ] && native=$(_cage_dsh_native); then
        /usr/bin/python3 /usr/local/lib/cage/dsh-ssh.py reconnect "$native"
      fi
    fi
    return "$status"
  }

  case $- in
    *i*)
      if [ -z "${_cage_dsh_prompt_installed:-}" ]; then
        _cage_dsh_prompt_installed=1
        if [ -n "${BASH_VERSION:-}" ]; then
          case "$(declare -p PROMPT_COMMAND 2>/dev/null)" in
            'declare -a '*) eval 'PROMPT_COMMAND+=(_cage_dsh_first_prompt)' ;;
            *) PROMPT_COMMAND="_cage_dsh_first_prompt${PROMPT_COMMAND:+; $PROMPT_COMMAND}" ;;
          esac
        elif [ -n "${ZSH_VERSION:-}" ]; then
          autoload -Uz add-zsh-hook
          add-zsh-hook precmd _cage_dsh_first_prompt
        fi
      fi
      ;;
  esac
fi
CAGE_DSH_PROFILE
for rc in /etc/zsh/zshenv /etc/bash.bashrc; do
  if [ -f "$rc" ] && ! grep -qs '/etc/profile.d/cage-dsh.sh' "$rc"; then
    echo '[ -r /etc/profile.d/cage-dsh.sh ] && . /etc/profile.d/cage-dsh.sh' | sudo tee -a "$rc" >/dev/null
  fi
done
# OpenSSH supplies a per-login authenticated key file for direct SSH.
sshd_config=/etc/ssh/sshd_config.d/80-cage-dsh-authinfo.conf
if [ -d /etc/ssh/sshd_config.d ] && [ -x /usr/sbin/sshd ] && [ ! -e "$sshd_config" ] &&
   sudo /usr/sbin/sshd -t >/dev/null 2>&1; then
  printf '%s\n' 'ExposeAuthInfo yes' | sudo tee "$sshd_config" >/dev/null
  if sudo /usr/sbin/sshd -t >/dev/null 2>&1; then
    sudo systemctl reload ssh.service >/dev/null 2>&1 || :
  else
    sudo rm -f "$sshd_config"
  fi
fi
`
}
