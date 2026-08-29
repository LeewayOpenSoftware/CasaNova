#!/bin/bash


GetSysInfo() {
  if [ -s "/etc/redhat-release" ]; then
    SYS_VERSION=$(cat /etc/redhat-release)
  elif [ -s "/etc/issue" ]; then
    SYS_VERSION=$(cat /etc/issue)
  fi
  SYS_INFO=$(uname -a)
  SYS_BIT=$(getconf LONG_BIT)
  MEM_TOTAL=$(free -m | grep Mem | awk '{print $2}')
  CPU_INFO=$(getconf _NPROCESSORS_ONLN)

  echo -e ${SYS_VERSION}
  echo -e Bit:${SYS_BIT} Mem:${MEM_TOTAL}M Core:${CPU_INFO}
  echo -e ${SYS_INFO}
}


GetNetCard() {
  if [ "$1" == "1" ]; then
    if [ -d "/sys/devices/virtual/net" ]; then
      ls /sys/devices/virtual/net
    fi
  else
    if [ -d "/sys/devices/virtual/net" ] && [ -d "/sys/class/net" ]; then
      ls /sys/class/net/ | grep -v "$(ls /sys/devices/virtual/net/)" -w
    fi
  fi
}


GetTimeZone(){
  timedatectl | grep "Time zone" | awk '{printf $3}'
}


#param 
CatNetCardState() {
  if [ -e "/sys/class/net/$1/operstate" ]; then
    cat /sys/class/net/$1/operstate
  fi
}

#docker
GetDockerRootDir() {
  if hash docker 2>/dev/null; then
    docker info | grep 'Docker Root Dir' | awk -F ':' '{print $2}'
  else
    echo ""
  fi
}


#param 
DelAppConfigDir() {
  if [ -d $1 ]; then
    rm -fr $1
  fi
}

#zerotier
#result start,end,sectors
GetLocalJoinNetworks() {
  zerotier-cli listnetworks -j
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


#param    /dev/sdb
#param 
DelPartition() {
  fdisk $1 <<EOF
  d
  $2
  wq
EOF
}


#param    /dev/sdb
#param 
AddPartition() {

  DelPartition $1
  parted -s $1 mklabel gpt

  parted -s $1 mkpart primary ext4 0 100%
  P=`lsblk -r $1 | sort | grep part | head -n 1 | awk '{print $1}'`
  mkfs.ext4 -m 1 -F /dev/${P}

  partprobe $1

}


GetDiskType() {
  fdisk $1 -l | grep Disklabel | awk -F: '{print $2}'
}


#param  /dev/sda
GetPlugInDisk() {
  fdisk -l | grep 'Disk' | grep 'sd' | awk -F , '{print substr($1,11,3)}'
}




#param   /dev/sda
#result bytes
#result sectors
GetDiskSizeAndSectors() {
  fdisk $1 -l | grep "$1:" | awk -F, 'BEGIN {OFS="\n"}{print $2,$3}' | awk '{print $1}'
}


#param   /dev/sda
#result start,end,sectors
GetPartitionSectors() {
  fdisk $1 -l | grep "$1[1-9]" | awk 'BEGIN{OFS=","}{print $1,$2,$3,$4}'
}


AutoRemoveUnuseDir() {
  DIRECTORY="/DATA/"
  dir=$(ls -l $DIRECTORY | grep "USB_Storage_sd[a-z][0-9]" | awk '/^d/ {print $NF}')
  for i in $dir; do

    path="$DIRECTORY$i"
    mountStr=$(mountpoint $path)
    notMountpoint="is not a mountpoint"
    if [[ $mountStr =~ $notMountpoint ]]; then
      if [ "$(ls -A $path)" = "" ]; then
        rm -fr $path
      else
        echo "$path is not empty"
      fi
    fi
  done
}

#samba
ReloadSamba() {
  /etc/init.d/smbd reload
}

# $1=sda1
# $2=volume{1}
do_mount() {
  DEVBASE=$1
  DEVICE="${DEVBASE}"
  # See if this drive is already mounted, and if so where
  MOUNT_POINT=$(lsblk -o name,mountpoint | grep ${DEVICE} | awk '{print $2}')

  if [ -n "${MOUNT_POINT}" ]; then
    ${log} "Warning: ${DEVICE} is already mounted at ${MOUNT_POINT}"
    exit 1
  fi

  # Get info for this drive: $ID_FS_LABEL and $ID_FS_TYPE
  eval $(blkid -o udev ${DEVICE} | grep -i -e "ID_FS_LABEL" -e "ID_FS_TYPE")

  LABEL=$2
  if grep -q " ${LABEL} " /etc/mtab; then
    # Already in use, make a unique one
    LABEL+="-${DEVBASE}"
  fi
  DEV_LABEL="${LABEL}"

  # Use the device name in case the drive doesn't have label
  if [ -z ${DEV_LABEL} ]; then
    DEV_LABEL="${DEVBASE}"
  fi

  MOUNT_POINT="${DEV_LABEL}"

  ${log} "Mount point: ${MOUNT_POINT}"

  mkdir -p ${MOUNT_POINT}

  case ${ID_FS_TYPE} in
  vfat)
    mount -t vfat -o rw,relatime,users,gid=100,umask=000,shortname=mixed,utf8=1,flush ${DEVICE} ${MOUNT_POINT}
    ;;
  ext[2-4])
    mount -o noatime ${DEVICE} ${MOUNT_POINT} >/dev/null 2>&1
    ;;
  exfat)
    mount -t exfat ${DEVICE} ${MOUNT_POINT} >/dev/null 2>&1
    ;;
  ntfs)
    ntfs-3g ${DEVICE} ${MOUNT_POINT}
    ;;
  iso9660)
    mount -t iso9660 ${DEVICE} ${MOUNT_POINT}
    ;;
  *)
    /bin/rmdir "${MOUNT_POINT}"
    exit 0
    ;;
  esac
}

# $1=sda1
do_umount() {
  log="logger -t usb-mount.sh -s "
  DEVBASE=$1
  DEVICE="${DEVBASE}"
  MOUNT_POINT=$(mount | grep ${DEVICE} | awk '{ print $3 }')

  if [[ -z ${MOUNT_POINT} ]]; then
    ${log} "Warning: ${DEVICE} is not mounted"
  else
    /bin/kill -9 $(lsof ${MOUNT_POINT})
    umount -l ${DEVICE}
    ${log} "Unmounted ${DEVICE} from ${MOUNT_POINT}"
    if [ "`ls -A ${MOUNT_POINT}`" = "" ]; then
      /bin/rm -fr "${MOUNT_POINT}"
    fi
    
    sed -i.bak "\@${MOUNT_POINT}@d" /var/log/usb-mount.track
  fi

}
# $1=/mnt/volume1/data.img
# $2=100G
PackageDocker() {
  image=$1
  docker="/mnt/casa_docker"
  #docker,

  if [ ! -d "$docker" ]; then
    mkdir ${docker}
  fi

  if [ "$(ls -A $docker)" = "" ]; then
    echo "$docker count is 0"
  else
    mkdir ${docker}_bak
    mv -r ${docker} ${docker}_bak
  fi

  daemon="/etc/docker/daemon.json"
  #1img
  fallocate -l $2 $image
  #2img
  mkfs -t ext4 $image
  #3img
  sudo mount -o loop $image $docker
  #4/var/lib/dockerimg
  systemctl stop docker.socket
  systemctl stop docker
  cp -r /var/lib/docker/* ${docker}/
  #5/etc/dockerdaemon.json()
  if [ -d "$daemon" ]; then
    mv -r $daemon ${daemon}.bak
  fi
  echo "{\"data-root\": \"$docker\"}" >$daemon

  #rm -fr /var/lib/docker
  systemctl start docker.socket
  systemctl start docker
}

DockerImgMove() {
  image=$1
  systemctl stop docker.socket
  systemctl stop docker
  sudo umount -f $image
}

GetDockerDataRoot() {
  docker info | grep "Docker Root Dir:"
}

SetLink() {
  ln -s /mnt/casa_sda1/AppData /DATA/AppData

  find /DATA -type l -delete
}



TarFolder() {

  tar -zcvf data.tar.gz -C/DATA/ AppDataBak/


  tar zxvf data.tar.gz 


  ls /DATA/Media -lR | grep "^-" | wc -l
  # ls -lR|grep "^d"| wc -l 


  du -sh /DATA
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

# restart samba service
RestartSMBD(){
  $sudo_cmd systemctl restart smbd
}

# edit user password $1:username
EditSmabaUserPassword(){
  $sudo_cmd smbpasswd $1
}

AddSmabaUser(){
  $sudo_cmd useradd $1
  $sudo_cmd smbpasswd -a $1 <<EOF
    $2
    $2
EOF
}

# $1:username $2:host $3:share $4:port $5:mountpoint $6:password 
MountCIFS(){
 $sudo_cmd mount -t cifs -o username=$1,password=$6,port=$4 //$2/$3 $5
}

UDEVILUmount(){
  $sudo_cmd udevil umount -f $1
}