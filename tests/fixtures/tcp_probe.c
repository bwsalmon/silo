// Adversarial TCP peer used only inside real OCI/Bubblewrap acceptance fixtures.
#define _GNU_SOURCE
#include <arpa/inet.h>
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <netdb.h>
#include <pthread.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#define REQUIRE(x) do { if (!(x)) { fprintf(stderr,"FAIL line=%d errno=%d\n",__LINE__,errno); exit(91); } } while (0)
static double seconds(void) {
    struct timespec t; REQUIRE(!clock_gettime(CLOCK_MONOTONIC,&t));
    return t.tv_sec + t.tv_nsec / 1e9;
}
static void bound(int fd, int seconds) {
    struct timeval t = {.tv_sec=seconds};
    REQUIRE(!setsockopt(fd,SOL_SOCKET,SO_RCVTIMEO,&t,sizeof(t)));
    REQUIRE(!setsockopt(fd,SOL_SOCKET,SO_SNDTIMEO,&t,sizeof(t)));
}
static int dial(const char *host, int port) {
    char text[16]; snprintf(text,sizeof(text),"%d",port);
    struct addrinfo hints={.ai_family=AF_INET,.ai_socktype=SOCK_STREAM}, *addr=NULL;
    if (getaddrinfo(host,text,&hints,&addr)) return -1;
    int fd=socket(AF_INET,SOCK_STREAM|SOCK_CLOEXEC,0);
    REQUIRE(fd>=0); bound(fd,20);
    int ok=connect(fd,addr->ai_addr,addr->ai_addrlen);
    freeaddrinfo(addr);
    if(ok) { close(fd); return -1; }
    return fd;
}
static void write_all(int fd, const void *data, size_t n) {
    const char *p=data;
    while(n) {
        ssize_t r=send(fd,p,n,MSG_NOSIGNAL);
        REQUIRE(r>0); p+=r; n-=r;
    }
}
static void read_exact(int fd, const char *expected, size_t n) {
    char buf[8192];
    while(n) {
        ssize_t r=recv(fd,buf,n<sizeof(buf)?n:sizeof(buf),0);
        REQUIRE(r>0 && !memcmp(buf,expected,(size_t)r));
        expected+=r; n-=r;
    }
}
static void no_fds(void) {
    DIR *d=opendir("/proc/self/fd"); REQUIRE(d);
    int own=dirfd(d); struct dirent *e;
    while((e=readdir(d))) {
        if(e->d_name[0]=='.') continue;
        int fd=atoi(e->d_name);
        REQUIRE(fd<=2 || fd==own);
    }
    closedir(d);
}
static void peer(int fd) {
    char mode; REQUIRE(recv(fd,&mode,1,MSG_WAITALL)==1);
    if(mode=='R') {
        struct linger l={.l_onoff=1,.l_linger=0};
        REQUIRE(!setsockopt(fd,SOL_SOCKET,SO_LINGER,&l,sizeof(l)));
        close(fd); _exit(0);
    }
    if(mode=='V') {
        int reverse=dial("127.77.0.1",8080); REQUIRE(reverse<0);
        write_all(fd,"reverse=denied",14); shutdown(fd,SHUT_WR); close(fd); _exit(0);
    }
    if(mode=='S') { sleep(25); close(fd); _exit(0); } // Never read or write payload.
    char buf[8192]; ssize_t n;
    while((n=recv(fd,buf,sizeof(buf),0))>0) write_all(fd,buf,(size_t)n);
    REQUIRE(n==0);
    if(mode=='H') write_all(fd,"after-eof",9);
    REQUIRE(!shutdown(fd,SHUT_WR)); close(fd); _exit(0);
}
static int serve(int port) {
    int fd=socket(AF_INET,SOCK_STREAM|SOCK_CLOEXEC,0); REQUIRE(fd>=0);
    int one=1; REQUIRE(!setsockopt(fd,SOL_SOCKET,SO_REUSEADDR,&one,sizeof(one)));
    struct sockaddr_in addr={.sin_family=AF_INET,.sin_port=htons((uint16_t)port),.sin_addr.s_addr=htonl(INADDR_LOOPBACK)};
    REQUIRE(!bind(fd,(struct sockaddr*)&addr,sizeof(addr))); REQUIRE(!listen(fd,16));
    signal(SIGCHLD,SIG_IGN);
    for(;;) {
        int c=accept4(fd,NULL,NULL,SOCK_CLOEXEC); REQUIRE(c>=0);
        pid_t p=fork(); REQUIRE(p>=0);
        if(!p) { close(fd); bound(c,30); peer(c); }
        close(c);
    }
}
static void tiny(const char *host) {
    int fd=dial(host,9000); REQUIRE(fd>=0);
    write_all(fd,"Erequest",8); REQUIRE(!shutdown(fd,SHUT_WR));
    read_exact(fd,"request",7);
    char c; REQUIRE(recv(fd,&c,1,0)==0);
    close(fd);
}
struct writer { int fd; size_t size; };
static void *bulk_write(void *arg) {
    struct writer *w=arg; char buf[8192]; memset(buf,0x5a,sizeof(buf));
    size_t n=w->size;
    while(n) { size_t part=n<sizeof(buf)?n:sizeof(buf); write_all(w->fd,buf,part); n-=part; }
    REQUIRE(!shutdown(w->fd,SHUT_WR)); return NULL;
}
static void bulk(const char *host, size_t size) {
    int fd=dial(host,9000); REQUIRE(fd>=0); write_all(fd,"E",1);
    struct writer w={fd,size}; pthread_t writer;
    REQUIRE(!pthread_create(&writer,NULL,bulk_write,&w));
    char buf[8192]; size_t total=0; ssize_t n;
    while((n=recv(fd,buf,sizeof(buf),0))>0) {
        for(ssize_t i=0;i<n;i++) REQUIRE(buf[i]==0x5a);
        total+=(size_t)n;
    }
    REQUIRE(n==0 && total==size); REQUIRE(!pthread_join(writer,NULL)); close(fd);
}
static void denied(const char *host, int port) {
    int fd=dial(host,port); REQUIRE(fd<0);
}
static void wildcard(void) {
    int fd=socket(AF_INET,SOCK_STREAM|SOCK_CLOEXEC,0); REQUIRE(fd>=0);
    struct sockaddr_in addr={.sin_family=AF_INET,.sin_port=htons(8080),.sin_addr.s_addr=htonl(INADDR_ANY)};
    errno=0; REQUIRE(bind(fd,(struct sockaddr*)&addr,sizeof(addr))<0 && errno==EADDRINUSE);
    close(fd);
}
int main(int argc,char **argv) {
    REQUIRE(argc>=2); signal(SIGPIPE,SIG_IGN);
    no_fds();
    const char *mode=argv[1];
    if(!strcmp(mode,"server")) return serve(9000);
    if(!strcmp(mode,"direct-bench")) {
        pid_t p=fork(); REQUIRE(p>=0);
        if(!p) return serve(9000);
        usleep(100000);
        double t=seconds(); for(int i=0;i<32;i++) tiny("127.0.0.1");
        double short_time=seconds()-t;
        t=seconds(); bulk("127.0.0.1",4194304);
        printf("direct_short_32_seconds=%.6f direct_bulk_bytes=4194304 direct_bulk_seconds=%.6f\n",short_time,seconds()-t);
        kill(p,SIGKILL); REQUIRE(waitpid(p,NULL,0)==p); return 0;
    }
    if(!strcmp(mode,"denied")) {
        REQUIRE(argc==4); denied(argv[2],atoi(argv[3])); puts("denied=ok"); return 0;
    }
    if(!strcmp(mode,"conflict")) { wildcard(); puts("wildcard-conflict=ok"); return 0; }
    if(!strcmp(mode,"reverse")) {
        denied("127.77.0.1",8080); denied("127.77.0.2",8080);
        puts("reverse=denied"); return 0;
    }
    REQUIRE(argc>=3); const char *host=argv[2];
    if(!strcmp(mode,"reverse-live")) {
        int fd=dial(host,9000); REQUIRE(fd>=0); write_all(fd,"V",1);
        read_exact(fd,"reverse=denied",14); close(fd); puts("reverse-live=denied"); return 0;
    }
    if(!strcmp(mode,"half")) {
        int fd=dial(host,9000); REQUIRE(fd>=0);
        write_all(fd,"Hrequest",8); REQUIRE(!shutdown(fd,SHUT_WR));
        read_exact(fd,"requestafter-eof",16); char c; REQUIRE(recv(fd,&c,1,0)==0);
        close(fd); puts("half-close=ok"); return 0;
    }
    if(!strcmp(mode,"reset")) {
        int fd=dial(host,9000); REQUIRE(fd>=0);
        write_all(fd,"R",1); char buf[1]; ssize_t n=recv(fd,buf,1,0);
        REQUIRE(n==0 || (n<0 && errno==ECONNRESET)); close(fd);
        tiny(host); puts("reset-recovery=ok"); return 0;
    }
    if(!strcmp(mode,"repeat")) {
        double t=seconds(); for(int i=0;i<32;i++) tiny(host);
        double short_time=seconds()-t;
        t=seconds(); bulk(host,4194304);
        printf("relay_short_32_seconds=%.6f relay_bulk_bytes=4194304 relay_bulk_seconds=%.6f\n",short_time,seconds()-t);
        puts("repeat-duplex=ok"); return 0;
    }
    if(!strcmp(mode,"parallel")) {
        for(int i=0;i<4;i++) { pid_t p=fork(); REQUIRE(p>=0); if(!p) { bulk(host,1048576); _exit(0); } }
        for(int i=0;i<4;i++) { int status; REQUIRE(wait(&status)>0 && WIFEXITED(status) && WEXITSTATUS(status)==0); }
        puts("parallel=ok"); return 0;
    }
    if(!strcmp(mode,"hold") || !strcmp(mode,"backpressure")) {
        int fd=dial(host,9000); REQUIRE(fd>=0);
        write_all(fd,"S",1);
        // Wait for setup to complete; do not confuse an over-quota rejection with a live stream.
        usleep(200000);
        if(!strcmp(mode,"hold")) {
            printf("stream-open\n"); fflush(stdout);
            char c; ssize_t n=recv(fd,&c,1,0);
            REQUIRE(n==0 || (n<0 && errno==ECONNRESET)); puts("stream-closed"); close(fd); return 0;
        }
        int one=8192; REQUIRE(!setsockopt(fd,SOL_SOCKET,SO_SNDBUF,&one,sizeof(one)));
        bound(fd,20);
        char buf[8192]; memset(buf,'x',sizeof(buf));
        size_t total=0; double t=seconds(); ssize_t n;
        while((n=send(fd,buf,sizeof(buf),MSG_NOSIGNAL))>0) {
            total+=(size_t)n;
            REQUIRE(total<16777216); // Kernel and relay buffers cannot absorb an unbounded producer.
        }
        REQUIRE(total>0 && seconds()-t<19);
        printf("backpressure_bytes=%zu seconds=%.6f errno=%d\n",total,seconds()-t,errno);
        close(fd); return 0;
    }
    if(!strcmp(mode,"quota")) {
        int fds[4]; for(int i=0;i<4;i++) { fds[i]=dial(host,9000); REQUIRE(fds[i]>=0); write_all(fds[i],"S",1); }
        usleep(300000);
        int extra=dial(host,9000); REQUIRE(extra>=0);
        write_all(extra,"Eextra",6); char c; ssize_t n=recv(extra,&c,1,0);
        REQUIRE(n==0 || (n<0 && errno==ECONNRESET));
        close(extra); for(int i=0;i<4;i++) close(fds[i]);
        puts("quota=ok"); return 0;
    }
    return 92;
}
