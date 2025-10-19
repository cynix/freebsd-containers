#!/bin/sh

if [ ! -f /jdownloader/JDownloader.jar ]; then
  cp /bootstrap/JDownloader.jar /jdownloader/
fi

exec /usr/local/openjdk21-jre/bin/java -Djava.awt.headless=true -jar /jdownloader/JDownloader.jar -norestart
