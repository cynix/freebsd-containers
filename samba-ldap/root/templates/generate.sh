#!/bin/sh -eu

for conf in ldap nslcd smb4; do
  if [ ! -f /var/run/config/$conf.conf ]; then
    touch /var/run/config/$conf.conf && chmod 600 /var/run/config/$conf.conf

    sed \
      -e "s!__LDAP_URI__!$LDAP_URI!g" \
      -e "s!__LDAP_BASE__!$LDAP_BASE!g" \
      -e "s!__LDAP_USER_SUFFIX__!$LDAP_USER_SUFFIX!g" \
      -e "s!__LDAP_GROUP_SUFFIX__!$LDAP_GROUP_SUFFIX!g" \
      -e "s!__LDAP_MIN_UID__!$LDAP_MIN_UID!g" \
      -e "s!__LDAP_NSLCD_BINDDN__!$LDAP_NSLCD_BINDDN!g" \
      -e "s!__LDAP_NSLCD_BINDPW__!$LDAP_NSLCD_BINDPW!g" \
      -e "s!__LDAP_SAMBA_BINDDN__!$LDAP_SAMBA_BINDDN!g" \
      /templates/$conf.conf > /var/run/config/$conf.conf

    if [ -d /config/$conf.d/ ]; then
      for i in $(find /config/$conf.d/ -name '*.conf' | sort); do
        echo >> /var/run/config/$conf.conf
        echo "# $i" >> /var/run/config/$conf.conf
        cat "$i" >> /var/run/config/$conf.conf
      done
    fi
  fi
done
