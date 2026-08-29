#!/bin/bash
###
 # @Author: LinkLeong dev@lucentopen.org
 # @Date: 2022-11-15 15:51:44
 # @LastEditors: LinkLeong
 # @LastEditTime: 2022-11-15 15:53:37
 # @FilePath: /CasaOS/build/sysroot/usr/share/casaos/cleanup/script.d/03-cleanup-casaos.sh
 # @Description: 
 # @Website: https://casanova.dev
 # Copyright (c) 2022-2026 LucentOpen Software. 
### 

set -e

readonly APP_NAME_SHORT=casaos

__get_setup_script_directory_by_os_release() {
	pushd "$(dirname "${BASH_SOURCE[0]}")/../service.d/${APP_NAME_SHORT}" &>/dev/null

	{
		# shellcheck source=/dev/null
		{
			source /etc/os-release
			{
				pushd "${ID}"/"${VERSION_CODENAME}" &>/dev/null
			} || {
				pushd "${ID}" &>/dev/null
			} || {
                [[ -n ${ID_LIKE} ]] && for ID in ${ID_LIKE}; do
				    pushd "${ID}" >/dev/null && break
                done
			} || {
				echo "Unsupported OS: ${ID} ${VERSION_CODENAME} (${ID_LIKE})"
				exit 1
			}

			pwd

			popd &>/dev/null

		} || {
			echo "Unsupported OS: unknown"
			exit 1
		}

	}

	popd &>/dev/null
}

SETUP_SCRIPT_DIRECTORY=$(__get_setup_script_directory_by_os_release)

readonly SETUP_SCRIPT_DIRECTORY
readonly SETUP_SCRIPT_FILENAME="cleanup-${APP_NAME_SHORT}.sh"
readonly SETUP_SCRIPT_FILEPATH="${SETUP_SCRIPT_DIRECTORY}/${SETUP_SCRIPT_FILENAME}"

echo "[OK] Running ${SETUP_SCRIPT_FILENAME}..."
$SHELL "${SETUP_SCRIPT_FILEPATH}" "${BUILD_PATH}"
