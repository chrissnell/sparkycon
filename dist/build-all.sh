#!/bin/bash

platforms=( darwin linux freebsd windows openbsd )

for p in "${platforms[@]}"
do
   echo "Building for ${p}/amd64..."
   if [ "$p" = "windows" ]; then
      GOOS=$p GOARCH=amd64 go build -o sparkycon-win64.exe
      echo "Compressing..."
      zip -9 sparkycon-win64.zip sparkycon-win64.exe
      mv sparkycon-win64.zip binaries/
      rm sparkycon-win64.exe
   else
      OUT="sparkycon-${p}-amd64"
      GOOS=$p GOARCH=amd64 go build -o $OUT
      echo "Compressing..."
      gzip -f $OUT
      mv ${OUT}.gz binaries/
   fi
done
