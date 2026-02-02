#!/bin/bash

SERVER=${1:-1}
BASESTR=$(( 200 + (SERVER-1) ))

CMD=${2:-run}

if [[ $CMD == 'run' ]]; then

  docker run -it --rm \
	 --cap-add NET_ADMIN \
	 --name infinite-streaming${SERVER} \
	 -p "${BASESTR}80-${BASESTR}99:${BASESTR}80-${BASESTR}99" \
   -v $(pwd)/../dynamic_content:/dynamic_content \
	 infinite-streaming ${SERVER}

elif [[ $CMD == 'stop' ]]; then

  docker stop infinite-streaming${SERVER}

else

  echo >/dev/stderr "Unknown cmd: $CMD"
  echo >/dev/stderr "usage: boss.sh [<server-num> [<cmd>]]"
  echo >/dev/stderr "options:"
  echo >/dev/stderr "  <server-num>   Multiple servers may be run."
  echo >/dev/stderr "                 Provide a number from 1 - 50."
  echo >/dev/stderr "                 Base port: ((200+num-1)*100)+80"
  echo >/dev/stderr "  <cmd>          Should be 'run' (default) or 'stop'"
  exit 1

fi
