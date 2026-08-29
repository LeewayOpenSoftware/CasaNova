#!/bin/bash

UDEVILUmount(){
  $sudo_cmd udevil umount -f $1
}


#param  /dev/sda
GetPlugInDisk() {
  fdisk -l | grep 'Disk' | grep 'sd' | awk -F , '{print substr($1,11,3)}'
}

#fat32
#param  /dev/sda1
#param 
FormatDisk() {
  if [ "$2" == "fat32" ]; then
    mkfs.vfat -F 32 $1
  elif [ "$2" == "ntfs" ]; then
    mkfs.ntfs $1
  elif [ "$2" == "ext4" ]; then
    mkfs.ext4 -m 1 -F $1
  elif [ "$2" == "exfat" ]; then
    mkfs.exfat $1
  else
    mkfs.ext4 -m 1 -F $1
  fi
}

#,
UMountPointAndRemoveDir() {
  set -e
  DEVICE=$1
  MOUNT_POINT=$(mount | grep ${DEVICE} | awk '{ print $3 }')
  if [[ -z ${MOUNT_POINT} ]]; then
    echo "Warning: ${DEVICE} is not mounted"
  else
    umount -lf ${DEVICE}
    /bin/rmdir "${MOUNT_POINT}"
  fi
}


#param    /dev/sdb
#param 
AddPartition() {
  set -e

  parted -s $1 mklabel gpt

  parted -s $1 mkpart primary ext4 0 100%
  P=`lsblk -r $1 | sort | grep part | head -n 1 | awk '{print $1}'`
  mkfs.ext4 -m 1 -F /dev/${P}

  partprobe $1
}


GetDiskType() {
  fdisk $1 -l | grep Disklabel | awk -F: '{print $2}'
}

# $1=sda1
# $2=volume{1}
# $3=extra mount options (optional, comma-separated, e.g. discard,noatime)
#    Wave 34-X residual: SSDs detected by the Go side need TRIM enabled
#    via `discard`. The Go helper passes this through; if absent we
#    behave exactly as before.
do_mount() {
  set -e

  DEVBASE=$1
  DEVICE="${DEVBASE}"
  EXTRA_OPTS="${3:-}"
  # See if this drive is already mounted, and if so where
  MOUNT_POINT=$(lsblk -o mountpoint -nr "${DEVICE}" | head -n 1)

  if [ -n "${MOUNT_POINT}" ]; then
    echo "${DEVICE} is already mounted at ${MOUNT_POINT}"
    exit 1
  fi

  # Get info for this drive: $ID_FS_LABEL and $ID_FS_TYPE
  DRIVE_INFO=$(blkid -o udev "${DEVICE}" | grep -i -e "ID_FS_LABEL" -e "ID_FS_TYPE") || {
    echo "${DEVICE} does not have a filesystem or it might be corrupted. Please consider format it."
    exit 1
  }

  eval "${DRIVE_INFO}"

  LABEL=$2
  if grep -q " ${LABEL} " /etc/mtab; then
    # Already in use, make a unique one
    LABEL+="-${DEVBASE}"
  fi
  DEV_LABEL="${LABEL}"

  # Use the device name in case the drive doesn't have label
  if [ -z "${DEV_LABEL}" ]; then
    DEV_LABEL="${DEVBASE}"
  fi

  MOUNT_POINT="${DEV_LABEL}"

  echo "Mount point: ${MOUNT_POINT}"

  mkdir -p "${MOUNT_POINT}"

  # Build the option string for filesystems that already pass `-o`. We
  # only append EXTRA_OPTS for the ext family — other filesystems either
  # have a fixed option set (vfat) or use a different driver (ntfs-3g)
  # where the discard semantics are different.
  case ${ID_FS_TYPE} in
  vfat)
    mount -t vfat -o rw,relatime,users,gid=100,umask=000,shortname=mixed,utf8=1,flush "${DEVICE}" "${MOUNT_POINT}"
    ;;
  ext[2-4])
    EXT_OPTS="noatime"
    if [ -n "${EXTRA_OPTS}" ]; then
      EXT_OPTS="${EXT_OPTS},${EXTRA_OPTS}"
    fi
    mount -o "${EXT_OPTS}" "${DEVICE}" "${MOUNT_POINT}"
    ;;
  exfat)
    mount -t exfat "${DEVICE}" "${MOUNT_POINT}"
    ;;
  ntfs)
    ntfs-3g "${DEVICE}" "${MOUNT_POINT}"
    ;;
  iso9660)
    mount -t iso9660 "${DEVICE}" "${MOUNT_POINT}"
    ;;
  *)
    echo "Unsupported filesystem type: ${ID_FS_TYPE}"
    /bin/rmdir "${MOUNT_POINT}"
    exit 1
    ;;
  esac
}

# $1=sda1
do_umount() {
  DEVBASE=$1
  DEVICE="${DEVBASE}"
  MOUNT_POINT=$(mount | grep ${DEVICE} | awk '{ print $3 }')

  if [[ -z ${MOUNT_POINT} ]]; then
    echo "Warning: ${DEVICE} is not mounted"
  else
    /bin/kill -9 $(lsof ${MOUNT_POINT})
    umount -l ${DEVICE}
    echo "Unmounted ${DEVICE} from ${MOUNT_POINT}"
    if [ "`ls -A ${MOUNT_POINT}`" = "" ]; then
      /bin/rm -fr "${MOUNT_POINT}"
    fi
    
    sed -i.bak "\@${MOUNT_POINT}@d" /var/log/usb-mount.track
  fi

}

USB_Start_Auto() {
  ((EUID)) && sudo_cmd="sudo"
  $sudo_cmd systemctl enable devmon@devmon
  $sudo_cmd systemctl start devmon@devmon
}

USB_Stop_Auto() {
  ((EUID)) && sudo_cmd="sudo"
  $sudo_cmd systemctl stop devmon@devmon
  $sudo_cmd systemctl disable devmon@devmon
  $sudo_cmd udevil clean
}

GetDeviceTree(){  
  cat /proc/device-tree/model
}