# -*- mode: ruby -*-
# vi: set ft=ruby :

Vagrant.configure("2") do |config|
  config.vm.define "gopheros-build" do |v|
  end

  config.vm.provider "virtualbox" do |vb|
    vb.customize ["modifyvm", :id, "--usb", "on"]
    vb.customize ["modifyvm", :id, "--usbehci", "off"]
    vb.customize ["modifyvm", :id, "--cableconnected1", "on"]
  end

  config.vm.box = "minimal/xenial64"

  config.vm.synced_folder "./", "/home/vagrant/workspace"

  config.vm.provision "shell", inline: <<-SHELL
    apt-get update
    apt-get install -y nasm make xorriso binutils gcc
    [ ! -d "/usr/local/go" ] && wget -qO- https://storage.googleapis.com/golang/go1.8.3.linux-amd64.tar.gz | tar xz -C /usr/local
    mkdir -p /home/vagrant/go/src
    mkdir -p /home/vagrant/go/bin
    mkdir -p /home/vagrant/go/pkg
    chown -R vagrant:vagrant /home/vagrant/go
    echo "export GOROOT=/usr/local/go" > /etc/profile.d/go.sh
    echo "export GOBIN=/usr/local/go/bin" >> /etc/profile.d/go.sh
    echo "export GOPATH=/home/vagrant/go" >> /etc/profile.d/go.sh
    echo "export PATH=$PATH:/usr/local/go/bin" >> /etc/profile.d/go.sh
  SHELL
end
