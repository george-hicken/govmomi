# A minimal debian image with systemd as PID 1, for the simulator's RUN.vmci
# tests. vmci-test-writer.service writes guestinfo.vmci-systemd-test=pass
# through /usr/bin/vmware-rpctool, which vcsim mounts at container start; the
# image does not contain it.
#
# The tests build it themselves:
#   docker build -t localhost/vmci-systemd-init:latest simulator/testdata/vmci-systemd-init

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        systemd \
        systemd-sysv \
        dbus \
        procps \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Units that only delay boot in a container.
RUN systemctl mask \
        getty@tty1.service \
        getty.target \
        console-getty.service \
        systemd-networkd-wait-online.service \
        systemd-resolved.service \
        apt-daily.service \
        apt-daily-upgrade.service \
        apt-daily.timer \
        apt-daily-upgrade.timer \
        e2scrub_reap.service

COPY vmci-test-writer.service /etc/systemd/system/vmci-test-writer.service
RUN systemctl enable vmci-test-writer.service

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
