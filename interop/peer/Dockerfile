# Copyright 2026 Abhinav Gautam
# SPDX-License-Identifier: Apache-2.0
#
# mobilityhouse/ocpp (MIT, github.com/mobilityhouse/ocpp) as an interop
# peer; it is installed from PyPI at build time, not redistributed.
FROM python:3.12.10-slim
RUN pip install --no-cache-dir ocpp==2.1.0 websockets==15.0.1
COPY csms.py /peer/csms.py
WORKDIR /peer
