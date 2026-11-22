// Static test workload only. No part of this program is trusted by the runner.
#define _GNU_SOURCE
#include <arpa/inet.h>
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <sched.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mount.h>
#include <sys/prctl.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>
#include <net/if.h>
#define CHECK(test) do { if (!(test)) { printf("FAIL line=%d errno=%d\n", __LINE__, errno); fflush(stdout); return 90; } } while (0)
static double seconds(clockid_t clock) {
    struct timespec t; clock_gettime(clock, &t);
    return t.tv_sec + t.tv_nsec / 1e9;
}
static void namespaces(void) {
    char p[128], v[128];
    const char *names[] = {"pid", "mnt", "net"};
    for (int i=0;i<3;i++) {
        snprintf(p,sizeof(p),"/proc/self/ns/%s",names[i]);
        ssize_t n=readlink(p,v,sizeof(v)-1);
        if(n>=0) { v[n]=0; printf("%s=%s\n",names[i],v); }
    }
    fflush(stdout);
}
static int denied_connect(const char *address, int port) {
    int fd=socket(AF_INET,SOCK_STREAM|SOCK_NONBLOCK,0);
    if(fd<0) return 1;
    struct sockaddr_in a={.sin_family=AF_INET,.sin_port=htons(port)};
    inet_pton(AF_INET,address,&a.sin_addr);
    int result=connect(fd,(struct sockaddr*)&a,sizeof(a));
    if(result<0 && errno==EINPROGRESS) {
        fd_set set;FD_ZERO(&set);FD_SET(fd,&set);
        struct timeval timeout={.tv_sec=0,.tv_usec=200000};
        if(select(fd+1,NULL,&set,NULL,&timeout)>0) {
            int error=0;socklen_t size=sizeof(error);
            getsockopt(fd,SOL_SOCKET,SO_ERROR,&error,&size);
            result=error ? -1 : 0;
        }
    }
    close(fd);return result!=0;
}
int main(int argc,char **argv) {
    setbuf(stdout,NULL);
    CHECK(argc>=2);
    const char *mode=argv[1];
    if(!strcmp(mode,"check")) {
        CHECK(argc==4);
        CHECK(getuid()==0 && getgid()==0);
        gid_t groups[32];int count=getgroups(32,groups);CHECK(count>=0);
        for(int i=0;i<count;i++)CHECK(groups[i]==0);
        for(int fd=3;fd<1024;fd++) CHECK(fcntl(fd,F_GETFD)==-1 && errno==EBADF);
        CHECK(getenv("SILO_SENTINEL_SECRET")==NULL);
        CHECK(getenv("IMAGE") && !strcmp(getenv("IMAGE"),"from-image"));
        CHECK(getenv("VALUE") && !strcmp(getenv("VALUE"),"operator"));
        CHECK(open(argv[2],O_RDONLY)<0);
        CHECK(open("/proc/1/root/tmp/host-secret",O_RDONLY)<0);
        CHECK(open("/escape",O_WRONLY|O_CREAT,0600)<0 && errno==EROFS);
        CHECK(open("/dev/shm/escape",O_WRONLY|O_CREAT,0600)<0);
        CHECK(prctl(PR_GET_NO_NEW_PRIVS,0,0,0,0)==1);
        FILE *s=fopen("/proc/self/status","r");CHECK(s);
        char line[256];int caps=0,seccomp=0;
        while(fgets(line,sizeof(line),s)) {
            if(!strncmp(line,"CapEff:",7) || !strncmp(line,"CapPrm:",7) || !strncmp(line,"CapBnd:",7)) {
                CHECK(strtoull(line+7,NULL,16)==0);caps++;
            }
            if(!strncmp(line,"Seccomp:",8)) seccomp=atoi(line+8);
        }
        fclose(s);CHECK(caps==3 && seccomp==2);
        CHECK(unshare(CLONE_NEWUSER)<0 && errno==EPERM);
        CHECK(unshare(CLONE_NEWNET)<0 && errno==EPERM);
        int ns=open("/proc/self/ns/net",O_RDONLY);CHECK(ns>=0);
        CHECK(setns(ns,CLONE_NEWNET)<0 && errno==EPERM);close(ns);
        CHECK(syscall(SYS_clone,CLONE_NEWUSER|SIGCHLD,0,0,0,0)<0 && errno==EPERM);
        CHECK(syscall(SYS_clone3,NULL,0)<0 && errno==ENOSYS);
        CHECK(mount("tmpfs","/tmp","tmpfs",0,NULL)<0 && errno==EPERM);
        CHECK(socket(AF_VSOCK,SOCK_STREAM,0)<0 && errno==EPERM);
        CHECK(socket(AF_NETLINK,SOCK_RAW,0)<0 && errno==EPERM);
        CHECK(denied_connect("127.0.0.1",atoi(argv[3])));
        CHECK(denied_connect("192.0.2.1",443));
        int sock=socket(AF_INET,SOCK_DGRAM,0);CHECK(sock>=0);
        struct ifconf interfaces;char buffer[2048];
        interfaces.ifc_len=sizeof(buffer);interfaces.ifc_buf=buffer;
        CHECK(ioctl(sock,SIOCGIFCONF,&interfaces)==0);
        CHECK(interfaces.ifc_len==(int)sizeof(struct ifreq));
        CHECK(!strcmp(interfaces.ifc_req[0].ifr_name,"lo"));close(sock);
        CHECK(access("/old",F_OK)<0 && access("/opaque/stale",F_OK)<0);
        CHECK(access("/opaque/fresh",F_OK)==0 && access("/keep",F_OK)==0);
        int keep=open("/absolute-link",O_RDONLY);CHECK(keep>=0);close(keep);
        namespaces();
        usleep(500000);
        puts("check=ok");return 0;
    }
    if(!strcmp(mode,"config")) {
        char cwd[128];CHECK(getcwd(cwd,sizeof(cwd)));
        printf("arg=%s cwd=%s image=%s value=%s\n",argc>2?argv[2]:"none",cwd,getenv("IMAGE"),getenv("VALUE"));
        return 0;
    }
    if(!strcmp(mode,"disk")) {
        static char data[65536];memset(data,1,sizeof(data));
        size_t total=0;int fd=open("/tmp/fill",O_CREAT|O_WRONLY,0600);CHECK(fd>=0);
        ssize_t n;
        while((n=write(fd,data,sizeof(data)))>0)total+=n;
        CHECK(errno==ENOSPC);close(fd);
        CHECK(total>0 && total<=4194304);
        printf("scratch_bytes=%zu\n",total);return 0;
    }
    if(!strcmp(mode,"pids")) {
        int count=0;pid_t p;
        while((p=fork())>=0) { if(p==0) { for(;;)pause(); } count++; }
        CHECK(errno==EAGAIN && count>8 && count<64);
        printf("forks=%d\n",count);return 0; // Deliberately leave children behind.
    }
    if(!strcmp(mode,"memory")) {
        for(;;) { char *p=malloc(1024*1024);CHECK(p);memset(p,0xa5,1024*1024);asm volatile(""::"r"(p):"memory"); }
    }
    if(!strcmp(mode,"cpu")) {
        double wall=seconds(CLOCK_MONOTONIC),cpu=seconds(CLOCK_PROCESS_CPUTIME_ID);
        volatile unsigned long n=0;while(seconds(CLOCK_MONOTONIC)-wall<2.0)n++;
        double used=seconds(CLOCK_PROCESS_CPUTIME_ID)-cpu;
        printf("cpu_ms=%.0f loops=%lu\n",used*1000,n);CHECK(used<1.0);return 0;
    }
    if(!strcmp(mode,"tree") || !strcmp(mode,"orphan")) {
        pid_t child=fork();CHECK(child>=0);
        if(child==0) {
            setsid();if(fork()==0) { for(;;)pause(); }for(;;)pause();
        }
        namespaces();
        if(!strcmp(mode,"orphan")) { usleep(500000);return 0; }
        for(;;)pause();
    }
    if(!strcmp(mode,"sleep")) { namespaces();for(;;)pause(); }
    if(!strcmp(mode,"exit")) return 17;
    if(!strcmp(mode,"logs")) { for(int i=0;i<100000;i++)putchar('x');return 0; }
    return 91;
}
