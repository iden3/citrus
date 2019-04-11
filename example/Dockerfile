FROM debian:stretch

RUN apt-get update
RUN apt-get install -y ca-certificates

COPY install-python36.sh /var/tmp/install-python36.sh
RUN /var/tmp/install-python36.sh
RUN rm /var/tmp/install-python36.sh

RUN apt-get install -y python3-pip
RUN pip3 install matrix_client
RUN apt-get install -y mongodb
RUN apt-get install -y nodejs
