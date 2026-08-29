#!/bin/bash
###
 # @Author: LinkLeong dev@lucentopen.org
 # @Date: 2022-02-17 18:53:29
 # @LastEditors: a624669980@163.com a624669980@163.com
 # @LastEditTime: 2022-06-30 10:11:31
 # @FilePath: /get/assist.sh
 # @Description: 
 # @Website: https://casanova.dev
 # Copyright (c) 2022-2026 Leeway Open Software. 
### 

version_0_2_11() {
  sysctl -w net.core.rmem_max=2500000
}

version_0_2_11