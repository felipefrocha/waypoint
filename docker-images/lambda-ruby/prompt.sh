if test -n "$APP_NAME"; then
  # check if stdout is a terminal...
  if test -t 1; then

    # see if it supports colors...
    ncolors=$(tput colors)

    if test -n "$ncolors" && test "$ncolors" -ge 8; then
      # bold="$(tput bold)"
      # underline="$(tput smul)"
      # standout="$(tput smso)"
      normal="$(tput sgr0)"
      # black="$(tput setaf 0)"
      # red="$(tput setaf 1)"
      green="$(tput setaf 2)"
      # yellow="$(tput setaf 3)"
      blue="$(tput setaf 4)"
      # magenta="$(tput setaf 5)"
      # cyan="$(tput setaf 6)"
      # white="$(tput setaf 7)"
    fi
  fi

  if test -n "$blue"; then
    export PS1="${APP_NAME}:${blue}\\w ${green}$ ${normal}"
  else
    export PS1="${APP_NAME}:\w $ "
  fi
fi
