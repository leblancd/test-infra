# Copyright 2018 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

FROM gcr.io/k8s-testimages/kubekins-e2e:v20180102-0e2b24a0b-master
LABEL maintainer "leblancd@cisco.com"

# add env we can debug with the image name:tag
ARG IMAGE_ARG
ENV IMAGE=${IMAGE_ARG}

RUN apt-get update && apt-get install -y \
    git \
    wget \
    liblz4-1 && \
    apt-get clean

# Install docker-machine
RUN curl -L https://github.com/docker/machine/releases/download/v0.13.0/docker-machine-`uname -s`-`uname -m` >/tmp/docker-machine && install /tmp/docker-machine /usr/local/bin/docker-machine

ENV SHELL bash
WORKDIR /workspace
ADD runner /

# TODO: Remove the following COPY of the locally built kubetest binary
# after the kubekins-e2e base image has been rebuilt with a kubetest binary
# that includes the gce-dind changes from PR #7529. In the meantime, overwrite
# the kubekins kubetest with a gce-dind aware kubetest.
COPY kubetest /workspace

ENTRYPOINT ["/bin/bash", "/runner"]
